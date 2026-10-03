package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"light-prober/internal/protocol"
	"light-prober/internal/telemetry"
)

func TestFailedConfigPersistenceIsFatalAndPreservesQueue(t *testing.T) {
	ctx := context.Background()
	metrics, _ := telemetry.New(ctx, false, time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.Config{Version: 1, ProbeID: "sg", Monitors: []protocol.Monitor{}})
	}))
	defer server.Close()
	directory := t.TempDir()
	opts := Options{Server: server.URL, Token: "test", DataDir: directory, Interval: time.Second, FlushInterval: time.Second, ConfigInterval: time.Second, Concurrency: 1, MaxQueueBytes: 1 << 20, AllowInsecure: true}
	a, err := New(opts, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.queue.Append(protocol.Result{MonitorID: "existing", Time: 1700000000, Up: true}); err != nil {
		t.Fatal(err)
	}
	// An existing directory makes replacing config.json fail on all platforms.
	if err := os.Mkdir(filepath.Join(directory, "config.json"), 0700); err != nil {
		t.Fatal(err)
	}
	err = a.refresh(ctx)
	var fatal fatalError
	if !errors.As(err, &fatal) {
		t.Fatalf("config persistence failure = %v, want fatal", err)
	}
	if a.identity != "" || a.config.ProbeID != "" {
		t.Fatal("identity became active before configuration was persisted")
	}
	if count, _, err := a.queue.Stats(); err != nil || count != 1 {
		t.Fatalf("prior queue not preserved: count=%d error=%v", count, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(directory, ".config-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary cache files leaked: %v, %v", leftovers, err)
	}
}
