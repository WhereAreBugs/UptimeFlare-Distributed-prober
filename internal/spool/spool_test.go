package spool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"light-prober/internal/protocol"

	bolt "go.etcd.io/bbolt"
)

func fixture() protocol.Result {
	return protocol.Result{MonitorID: "web", Time: 1700000000, Up: true, LatencyMS: 2.25}
}

func TestPersistenceFIFOAndPartialAcknowledgment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox", "results.db")
	q, err := Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		result := fixture()
		result.Time += int64(i)
		if err := q.Append(result); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := q.Peek(3)
	if err != nil || len(entries) != 3 {
		t.Fatalf("peek = %v, %v", entries, err)
	}
	if err := q.Ack(entries[:2]); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	remaining, err := q.Peek(200)
	if err != nil || len(remaining) != 3 || remaining[0].ID != entries[2].ID {
		t.Fatalf("remaining = %v, %v", remaining, err)
	}
	for i, entry := range remaining {
		if entry.Result.Time != fixture().Time+int64(i+2) {
			t.Fatalf("FIFO order changed: %v", remaining)
		}
	}
	count, size, err := q.Stats()
	if err != nil || count != 3 || size <= 0 {
		t.Fatalf("stats = %d/%d/%v", count, size, err)
	}
	if err := q.Ack(remaining); err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(remaining); err != nil {
		t.Fatalf("idempotent ack failed: %v", err)
	}
	count, size, err = q.Stats()
	if err != nil || count != 0 || size != 0 {
		t.Fatalf("empty stats = %d/%d/%v", count, size, err)
	}
	if err := q.Append(fixture()); err != nil {
		t.Fatal(err)
	}
	last, _ := q.Peek(1)
	if last[0].ID <= remaining[len(remaining)-1].ID {
		t.Fatal("sequence reused after queue emptied")
	}
}

func TestExactAcknowledgmentIsAtomic(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "results.db"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	for range 2 {
		if err := q.Append(fixture()); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := q.Peek(2)
	entries[1].Result.Time++
	if err := q.Ack(entries); !errors.Is(err, ErrMismatch) {
		t.Fatalf("mismatched ack = %v", err)
	}
	count, _, _ := q.Stats()
	if count != 2 {
		t.Fatal("partial deletion escaped rolled-back acknowledgment")
	}
	entries, _ = q.Peek(2)
	if err := q.Ack([]Entry{entries[0], entries[0]}); err != nil {
		t.Fatal(err)
	}
	count, _, _ = q.Stats()
	if count != 1 {
		t.Fatal("duplicate acknowledgment corrupted count")
	}
}

func TestFullQueueNeverDropsAndBoundsAllocations(t *testing.T) {
	data, _ := json.Marshal(fixture())
	q, err := Open(filepath.Join(t.TempDir(), "results.db"), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err := q.Append(fixture()); err != nil {
		t.Fatal(err)
	}
	if err := q.Append(fixture()); !errors.Is(err, ErrFull) {
		t.Fatalf("full append = %v", err)
	}
	count, size, err := q.Stats()
	if err != nil || count != 1 || size != int64(len(data)) {
		t.Fatalf("full stats = %d/%d/%v", count, size, err)
	}
	huge := fixture()
	huge.Message = strings.Repeat("x", maxResultBytes+1)
	if err := q.Append(huge); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized result = %v", err)
	}
	if _, err := q.Peek(201); err == nil {
		t.Fatal("unbounded peek allowed")
	}
}

func TestExclusiveFileLockAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "results.db")
	q, err := Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if second, err := Open(path, 1<<20); !errors.Is(err, ErrLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second open = %v", err)
	}
	for _, test := range []struct {
		path string
		mode os.FileMode
	}{{path, 0600}, {filepath.Dir(path), 0700}} {
		info, err := os.Stat(test.path)
		if err != nil || info.Mode().Perm() != test.mode {
			t.Fatalf("permissions for %s = %v, %v", test.path, info, err)
		}
	}
}

func TestCorruptEntriesFailClosed(t *testing.T) {
	for _, corrupt := range [][]byte{[]byte("not json"), []byte(`{"monitor_id":"web"}`), []byte(strings.Repeat("x", maxResultBytes+1))} {
		path := filepath.Join(t.TempDir(), "results.db")
		q, err := Open(path, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Append(fixture()); err != nil {
			t.Fatal(err)
		}
		if err := q.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(resultsKey).Put(encode(1), corrupt) }); err != nil {
			t.Fatal(err)
		}
		if items, err := q.Peek(1); !errors.Is(err, ErrCorrupt) || items != nil {
			t.Fatalf("corrupt peek = %v, %v", items, err)
		}
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
		if reopened, err := Open(path, 1<<20); !errors.Is(err, ErrCorrupt) {
			if reopened != nil {
				_ = reopened.Close()
			}
			t.Fatalf("corrupt reopen = %v", err)
		}
	}
}

func TestCorruptMetadataAndClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.db")
	q, err := Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(metaKey).Put(countKey, []byte("bad")) }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.Stats(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad metadata = %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Append(fixture()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed append = %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
}

func TestChecksumRejectsValidJSONBitCorruption(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "results.db"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err := q.Append(fixture()); err != nil {
		t.Fatal(err)
	}
	if err := q.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(resultsKey)
		data := append([]byte(nil), bucket.Get(encode(1))...)
		for i := recordHeader; i < len(data); i++ {
			if data[i] == '7' {
				data[i] = '8'
				break
			}
		}
		if !json.Valid(data[recordHeader:]) {
			t.Fatal("fixture modification is not valid JSON")
		}
		return bucket.Put(encode(1), data)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Peek(1); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("valid JSON corruption = %v", err)
	}
}

func TestLastTimeSurvivesRestartAndEmptyQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.db")
	q, err := Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	newest := fixture()
	newest.Time += 10
	if err := q.Append(newest); err != nil {
		t.Fatal(err)
	}
	if err := q.Append(fixture()); err != nil {
		t.Fatal(err)
	}
	entries, _ := q.Peek(2)
	if err := q.Ack(entries); err != nil {
		t.Fatal(err)
	}
	if last, err := q.LastTime(); err != nil || last != newest.Time {
		t.Fatalf("cursor after ACK = %d, %v", last, err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if last, err := q.LastTime(); err != nil || last != newest.Time {
		t.Fatalf("reopened empty cursor = %d, %v", last, err)
	}
	if count, _, err := q.Stats(); err != nil || count != 0 {
		t.Fatalf("reopened queue = %d, %v", count, err)
	}
}

func TestLastTimeMigrationAndValidation(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "results.db")
		q, err := Open(path, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Append(fixture()); err != nil {
			t.Fatal(err)
		}
		if err := q.db.Update(func(tx *bolt.Tx) error {
			if corrupt {
				return tx.Bucket(metaKey).Put(lastTimeKey, encode(uint64(fixture().Time-1)))
			}
			return tx.Bucket(metaKey).Delete(lastTimeKey)
		}); err != nil {
			t.Fatal(err)
		}
		_ = q.Close()
		q, err = Open(path, 1<<20)
		if corrupt {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("regressed cursor = %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if last, err := q.LastTime(); err != nil || last != fixture().Time {
			t.Fatalf("migrated cursor = %d, %v", last, err)
		}
		_ = q.Close()
	}
}

func TestFailedAppendDoesNotAdvanceLastTime(t *testing.T) {
	data, _ := json.Marshal(fixture())
	q, err := Open(filepath.Join(t.TempDir(), "results.db"), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err := q.Append(fixture()); err != nil {
		t.Fatal(err)
	}
	newer := fixture()
	newer.Time++
	if err := q.Append(newer); !errors.Is(err, ErrFull) {
		t.Fatalf("failed append = %v", err)
	}
	if last, err := q.LastTime(); err != nil || last != fixture().Time {
		t.Fatalf("uncommitted timestamp became active: %d, %v", last, err)
	}
}

func TestConcurrentAppendsPersistAllResults(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "results.db"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := q.Append(fixture()); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	count, _, err := q.Stats()
	if err != nil || count != 20 {
		t.Fatalf("concurrent count = %d, %v", count, err)
	}
}

func BenchmarkDurableAppendAndAck(b *testing.B) {
	q, err := Open(filepath.Join(b.TempDir(), "results.db"), 1<<20)
	if err != nil {
		b.Fatal(err)
	}
	defer q.Close()
	result := fixture()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := q.Append(result); err != nil {
			b.Fatal(err)
		}
		entries, err := q.Peek(1)
		if err != nil {
			b.Fatal(err)
		}
		if err := q.Ack(entries); err != nil {
			b.Fatal(err)
		}
	}
}
