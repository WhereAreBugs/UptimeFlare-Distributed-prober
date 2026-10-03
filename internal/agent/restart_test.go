package agent

import (
	"context"
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

func TestFastRestartDoesNotReuseSampleTime(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "acknowledged-empty"}[acknowledged], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
			defer server.Close()
			metrics, _ := telemetry.New(context.Background(), false, time.Minute)
			opts := Options{Server: server.URL, Token: "test", DataDir: t.TempDir(), Interval: time.Second, FlushInterval: time.Second, ConfigInterval: time.Second, Concurrency: 1, MaxQueueBytes: 1 << 20, AllowInsecure: true}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			a, err := New(opts, metrics, logger)
			if err != nil {
				t.Fatal(err)
			}
			cfg := protocol.Config{Version: 1, ProbeID: "sg", Monitors: []protocol.Monitor{{ID: "web", Target: server.URL}}}
			if err := a.save(cfg); err != nil {
				t.Fatal(err)
			}
			prior := protocol.Result{MonitorID: "web", Time: time.Now().Unix(), Up: true}
			if err := a.queue.Append(prior); err != nil {
				t.Fatal(err)
			}
			if acknowledged {
				entries, _ := a.queue.Peek(1)
				if err := a.queue.Ack(entries); err != nil {
					t.Fatal(err)
				}
			}
			a.Close()
			a, err = New(opts, metrics, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := a.round(ctx); err != nil {
				t.Fatal(err)
			}
			entries, err := a.queue.Peek(2)
			if err != nil || len(entries) == 0 {
				t.Fatalf("new round did not persist: %v, %v", entries, err)
			}
			newTime := entries[len(entries)-1].Result.Time
			if newTime <= prior.Time || newTime > time.Now().Unix() {
				t.Fatalf("restart sample time = %d, prior=%d wall=%d", newTime, prior.Time, time.Now().Unix())
			}
		})
	}
}

func TestBackwardClockWaitIsCancelableWithoutChecks(t *testing.T) {
	var checks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		w.WriteHeader(204)
	}))
	defer server.Close()
	metrics, _ := telemetry.New(context.Background(), false, time.Minute)
	opts := Options{Server: server.URL, Token: "test", DataDir: t.TempDir(), Interval: time.Second, FlushInterval: time.Second, ConfigInterval: time.Second, Concurrency: 1, MaxQueueBytes: 1 << 20, AllowInsecure: true}
	a, err := New(opts, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.config = protocol.Config{Version: 1, ProbeID: "sg", Monitors: []protocol.Monitor{{ID: "web", Target: server.URL}}}
	if err := a.queue.Append(protocol.Result{MonitorID: "web", Time: time.Now().Add(time.Hour).Unix(), Up: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := a.round(ctx); err != nil {
		t.Fatal(err)
	}
	if checks.Load() != 0 {
		t.Fatal("check executed before real time passed the persistent timestamp")
	}
	if count, _, err := a.queue.Stats(); err != nil || count != 1 {
		t.Fatalf("clock-wait changed queue: %d, %v", count, err)
	}
}
