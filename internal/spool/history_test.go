package spool

import (
	"errors"
	"light-prober/internal/protocol"
	"path/filepath"
	"strings"
	"testing"
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
