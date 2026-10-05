package spool

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"light-prober/internal/protocol"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestHistorySurvivesAcknowledgementRestartAndRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q, err := Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	const start int64 = 1700000100
	for i := range 147 {
		result := protocol.Result{MonitorID: "web", Time: start + int64(i)*300, Up: i%2 == 0, LatencyMS: 12, Message: "private response", Code: "dns_failed"}
		if err := q.Append(result); err != nil {
			t.Fatal(err)
		}
		entries, _ := q.Peek(200)
		if err := q.Ack(entries); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	now := start + 146*300
	history, err := q.History("web", now)
	if err != nil || len(history.Buckets) != 145 || history.Latest.Time != now || history.Latest.Message != "" {
		t.Fatalf("history = %+v, %v", history, err)
	}
	snapshot, err := q.Snapshot()
	if err != nil || snapshot.Count != 0 || snapshot.OldestAt != 0 || snapshot.LastAckAt == 0 {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	if err := q.PruneHistory([]string{"other"}); err != nil {
		t.Fatal(err)
	}
	history, err = q.History("web", now)
	if err != nil || history.Latest != nil || len(history.Buckets) != 0 {
		t.Fatal("removed assignment retained", err)
	}
}

func TestDailyHistoryUTCWindowsSuccessfulLatencyAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q, err := Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	const start int64 = 1700006400 // UTC midnight.
	for day := range 93 {
		for _, r := range []protocol.Result{
			{MonitorID: "web", Time: start + int64(day)*daySeconds + 1, Up: true, LatencyMS: 20},
			{MonitorID: "web", Time: start + int64(day)*daySeconds + daySeconds - 1, Up: false, LatencyMS: 5000},
		} {
			if err := q.Append(r); err != nil {
				t.Fatal(err)
			}
		}
		entries, _ := q.Peek(200)
		if err := q.Ack(entries); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	h, err := q.History("web", start+93*daySeconds-1)
	if err != nil || len(h.DailyBuckets) != 90 {
		t.Fatalf("daily window: %d, %v", len(h.DailyBuckets), err)
	}
	if h.DailyBuckets[0].Time != start+3*daySeconds {
		t.Fatal("UTC retention boundary incorrect")
	}
	for _, day := range h.DailyBuckets {
		if day.Checks != 2 || day.Failures != 1 || day.LatencyChecks != 1 || day.LatencySum != 20 {
			t.Fatalf("failed duration polluted daily latency: %+v", day)
		}
	}
	if err := q.Append(protocol.Result{MonitorID: "web", Time: start, Up: true, LatencyMS: 99}); err != nil {
		t.Fatal(err)
	}
	h, _ = q.History("web", start+93*daySeconds-1)
	if len(h.DailyBuckets) != 90 || h.DailyBuckets[0].Checks != 2 {
		t.Fatal("late expired sample changed retained day")
	}
	if err := q.PruneHistory([]string{"other"}); err != nil {
		t.Fatal(err)
	}
	h, err = q.History("web", start+93*daySeconds-1)
	if err != nil || len(h.DailyBuckets) != 0 {
		t.Fatal("removed target retained daily history", err)
	}
}

func TestDailyUpgradeRecoversQueueAndAcknowledgedHistoryWithoutDoubleCounting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q, err := Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	recent := now/300*300 - 300
	for _, r := range []protocol.Result{
		{MonitorID: "web", Time: now - 2*daySeconds, Up: true, LatencyMS: 30},
		{MonitorID: "web", Time: recent - 600 + 1, Up: true, LatencyMS: 20},
		{MonitorID: "web", Time: recent + 1, Up: true, LatencyMS: 10},
		{MonitorID: "web", Time: recent + 2, Up: false, LatencyMS: 5000},
	} {
		if err := q.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := q.Peek(200)
	if err := q.Ack(entries[1:3]); err != nil {
		t.Fatal(err)
	}
	if err := q.update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(dailyHistoryKey); err != nil {
			return err
		}
		bucket := tx.Bucket(historyKey).Bucket([]byte("web"))
		return bucket.ForEach(func(key, data []byte) error {
			if len(key) != 8 {
				return nil
			}
			var legacy map[string]any
			if err := json.Unmarshal(data, &legacy); err != nil {
				return err
			}
			delete(legacy, "latency_checks")
			if legacy["failures"].(float64) > 0 {
				legacy["latency_sum"] = 5010
			}
			encoded, err := json.Marshal(legacy)
			if err != nil {
				return err
			}
			return bucket.Put(key, encoded)
		})
	}); err != nil {
		t.Fatal(err)
	}
	digest := func() [32]byte {
		hash := sha256.New()
		if err := q.view(func(tx *bolt.Tx) error {
			return tx.Bucket(resultsKey).ForEach(func(key, value []byte) error { hash.Write(key); hash.Write(value); return nil })
		}); err != nil {
			t.Fatal(err)
		}
		return [32]byte(hash.Sum(nil))
	}
	before := digest()
	for range 2 { // Migration is idempotent on subsequent restarts.
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
		q, err = Open(path, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if digest() != before {
			t.Fatal("upgrade modified pending queue keys/values")
		}
		h, err := q.History("web", now)
		if err != nil {
			t.Fatal(err)
		}
		var checks, failures, latencyChecks uint64
		var latencySum float64
		for _, day := range h.DailyBuckets {
			checks += day.Checks
			failures += day.Failures
			latencyChecks += day.LatencyChecks
			latencySum += day.LatencySum
		}
		if checks != 4 || failures != 1 || latencyChecks != 2 || latencySum != 50 {
			t.Fatalf("incorrect legacy backfill: checks=%d failures=%d latencyChecks=%d latencySum=%v", checks, failures, latencyChecks, latencySum)
		}
	}
	q.Close()
}
func TestDatabaseBudgetStopsWithoutDroppingAndCanReuseAfterAck(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "queue.db"), MaxDatabaseBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	q.maxDatabaseBytes = 20 << 20
	count := 0
	for ; count < 1000; count++ {
		err = q.Append(protocol.Result{MonitorID: "web", Time: 1700000000 + int64(count), Message: strings.Repeat("x", 60000)})
		if errors.Is(err, ErrFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if count == 0 || count == 1000 {
		t.Fatal("disk guard did not stop within bounded fixture")
	}
	snapshot, err := q.Snapshot()
	if err != nil || snapshot.Count != uint64(count) || snapshot.DatabaseBytes > q.maxDatabaseBytes {
		t.Fatalf("size guard failed: %+v, %v", snapshot, err)
	}
	for {
		entries, err := q.Peek(200)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			break
		}
		if err := q.Ack(entries); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.Append(fixture()); err != nil {
		t.Fatal("freed pages not reusable", err)
	}
	snapshot, _ = q.Snapshot()
	if snapshot.DatabaseBytes > q.maxDatabaseBytes {
		t.Fatal("reusing pages exceeded budget")
	}
}
