package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"light-prober/internal/protocol"
	"light-prober/internal/telemetry"
)

func TestOfflineRestartAndLostAck(t *testing.T) {
	ctx := context.Background()
	var online atomic.Bool
	online.Store(true)
	var calls atomic.Int32
	var persisted atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !online.Load() {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/api/probes/config" {
			_ = json.NewEncoder(w).Encode(protocol.Config{Version: 1, ProbeID: "sg", Monitors: []protocol.Monitor{{ID: "web", Method: "GET", Target: target.URL}}})
			return
		}
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer gz.Close()
		var batch protocol.Batch
		if err = json.NewDecoder(gz).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		if calls.Add(1) == 1 {
			persisted.Add(int32(len(batch.Results)))
			w.WriteHeader(503)
			return
		} // committed, response lost
		_ = json.NewEncoder(w).Encode(protocol.Ack{BatchID: batch.BatchID, Accepted: len(batch.Results)})
	}))
	defer server.Close()
	metrics, _ := telemetry.New(ctx, false, time.Minute)
	opts := Options{Server: server.URL, Token: "test", DataDir: t.TempDir(), Interval: time.Minute, FlushInterval: 5 * time.Minute, ConfigInterval: 5 * time.Minute, Concurrency: 2, MaxQueueBytes: 1 << 20, Compress: true, AllowInsecure: true}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := New(opts, metrics, log)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	online.Store(false)
	if err = a.round(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = a.flush(ctx); err == nil {
		t.Fatal("offline upload succeeded")
	}
	a.Close()
	a, err = New(opts, metrics, log)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if len(a.config.Monitors) != 1 {
		t.Fatal("offline config not recovered")
	}
	if n, _, err := a.queue.Stats(); err != nil || n != 1 {
		t.Fatalf("restart n=%d err=%v", n, err)
	}
	online.Store(true)
	if _, err = a.flush(ctx); err == nil {
		t.Fatal("lost ACK accepted")
	}
	if n, _, _ := a.queue.Stats(); n != 1 {
		t.Fatal("deleted before matching ACK")
	}
	if _, err = a.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := a.queue.Stats(); n != 0 || persisted.Load() != 1 {
		t.Fatal("queue not drained or replay duplicates")
	}
}

func TestCacheReceiverIdentityBinding(t *testing.T) {
	ctx := context.Background()
	metrics, _ := telemetry.New(ctx, false, time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.Config{Version: 1, ProbeID: "sg", Monitors: []protocol.Monitor{}})
	}))
	defer server.Close()
	opts := Options{Server: server.URL, Token: "test", DataDir: t.TempDir(), Interval: time.Second, FlushInterval: time.Second, ConfigInterval: time.Second, Concurrency: 1, MaxQueueBytes: 1 << 20, AllowInsecure: true}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := New(opts, metrics, log)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	a.Close()
	opts.Server = "http://another.example"
	if a, err = New(opts, metrics, log); err == nil {
		a.Close()
		t.Fatal("data directory rebound to different receiver")
	}
}
