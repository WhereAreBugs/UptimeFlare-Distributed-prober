package agent

import (
	"context"
	"encoding/json"
	"io"
	"light-prober/internal/protocol"
	"light-prober/internal/telemetry"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFullQueueKeepsDashboardRunningAndCancelsCleanly(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer target.Close()
	metrics, _ := telemetry.New(context.Background(), false, time.Minute)
	opts := Options{Server: "https://receiver.test", Token: "fixture-token", DataDir: t.TempDir(), Interval: time.Minute, FlushInterval: time.Minute, ConfigInterval: time.Minute, Concurrency: 1, MaxQueueBytes: 1 << 20}
	a, err := New(opts, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r := protocol.Result{MonitorID: "fill", Time: time.Now().Unix() - 10, Message: strings.Repeat("x", 60000)}
	for a.queue.Append(r) == nil {
	}
	_, used, _ := a.queue.Stats()
	remaining := opts.MaxQueueBytes - used
	r.Message = "x"
	encoded, _ := json.Marshal(r)
	r.Message = strings.Repeat("x", int(remaining)-len(encoded)+1)
	if err := a.queue.Append(r); err != nil {
		t.Fatal(err)
	}
	before, _, _ := a.queue.Stats()
	a.config.Monitors = []protocol.Monitor{{ID: "web", Method: "GET", Target: target.URL}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.runChecksWithCapacity(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		state, _ := a.status()
		if state.CheckingPaused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("full queue did not pause")
		}
		time.Sleep(10 * time.Millisecond)
	}
	w := httptest.NewRecorder()
	a.webHandler(true).ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/status", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"checking_paused":true`) {
		t.Fatal("dashboard unavailable while full")
	}
	after, _, _ := a.queue.Stats()
	if after != before {
		t.Fatal("queued data discarded")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("capacity wait did not cancel")
	}
}

func TestReadOnlyDashboardOfflineSecretsAndPausedHistory(t *testing.T) {
	metrics, _ := telemetry.New(context.Background(), false, time.Minute)
	opts := Options{Server: "https://receiver.test", Token: "secret-probe-token", DataDir: t.TempDir(), Interval: time.Minute, FlushInterval: time.Minute, ConfigInterval: time.Minute, Concurrency: 1, MaxQueueBytes: 1 << 20}
	a, err := New(opts, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.config = protocol.Config{Version: 1, ProbeID: "private-id", Probe: protocol.Registration{Name: "Example ASN"}, Monitors: []protocol.Monitor{{ID: "a", Name: "A", Method: "GET", Target: "https://test/?token=secret-target", Headers: map[string]string{"Authorization": "secret-auth"}, Body: "private-body"}}, DisplayMonitors: []protocol.DisplayMonitor{{ID: "a", Name: "A", IntervalSeconds: 300}, {ID: "b", Name: "Paused", Paused: true, IntervalSeconds: 300}}}
	a.identity = a.config.ProbeID
	if err := a.queue.Append(protocol.Result{MonitorID: "b", Time: time.Now().Unix(), Up: false, Message: "private response", Stage: "dns"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := a.queue.Peek(200)
	if err := a.queue.Ack(entries); err != nil {
		t.Fatal(err)
	}
	handler := a.webHandler(true)
	call := func(path, host, method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://"+host+path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/", "/app.js", "/style.css", "/api/status", "/api/monitors?page=1", "/api/history?monitor=b"} {
		w := call(path, "localhost", http.MethodGet)
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		for _, secret := range []string{opts.Token, "secret-target", "secret-auth", "private-body", "private response", "private-id"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("%s leaked credential/identity", path)
			}
		}
	}
	w := call("/api/history?monitor=b", "localhost", http.MethodGet)
	var history struct {
		Latest *protocol.Result `json:"latest"`
	}
	if json.Unmarshal(w.Body.Bytes(), &history) != nil || history.Latest == nil {
		t.Fatal("acknowledged paused history unavailable")
	}
	if call("/api/history?monitor=not-assigned", "localhost", http.MethodGet).Code != 404 {
		t.Fatal("unassigned history exposed")
	}
	if call("/api/status", "attacker.test", http.MethodGet).Code != 403 {
		t.Fatal("DNS rebinding allowed")
	}
	if call("/api/status", "localhost", http.MethodPost).Code != 405 {
		t.Fatal("dashboard accepted write")
	}
	req := httptest.NewRequest("GET", "http://localhost/api/status", nil)
	req.Header.Set("Origin", "https://attacker.test")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("cross-origin allowed")
	}
	a.opts.WebPassword = "local-dashboard-secret"
	handler = a.webHandler(true)
	if call("/api/status", "localhost", http.MethodGet).Code != 401 {
		t.Fatal("authentication skipped")
	}
	req = httptest.NewRequest("GET", "http://localhost/api/status", nil)
	req.SetBasicAuth("probe", a.opts.WebPassword)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal("valid auth rejected")
	}
	a.opts.WebListen = "0.0.0.0:0"
	a.opts.WebPassword = ""
	if server, err := a.startWeb(context.Background()); err == nil {
		server.Close()
		t.Fatal("public listener without auth allowed")
	}
}
