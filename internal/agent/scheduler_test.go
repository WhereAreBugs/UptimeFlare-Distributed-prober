package agent

import (
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

func TestIndependentTargetCadences(t *testing.T) {
	base := time.Unix(1700000000, 0)
	plan := &schedule{}
	monitors := []protocol.Monitor{{ID: "fast", IntervalSeconds: 60}, {ID: "slow", IntervalSeconds: 300}}
	if err := plan.apply(monitors, time.Second, base, func(string) (int64, error) { return 0, nil }); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for second := 0; second <= 600; second++ {
		now := base.Add(time.Duration(second) * time.Second)
		for state := plan.due(now); state != nil; state = plan.due(now) {
			job := plan.started(state, context.Background(), now)
			counts[job.monitor.ID]++
			plan.completed(job, now.Add(10*time.Millisecond))
		}
	}
	if counts["fast"] != 11 || counts["slow"] != 3 {
		t.Fatalf("ten-minute cadence counts = %v, want fast11/slow3", counts)
	}
}

func TestChangedIntervalRecalculatesDeadlineAndSlowChecksSkipMissedPeriods(t *testing.T) {
	base := time.Unix(1700000000, 0)
	plan := &schedule{}
	monitor := protocol.Monitor{ID: "web", Target: "https://example.invalid", IntervalSeconds: 300}
	if err := plan.apply([]protocol.Monitor{monitor}, time.Second, base, func(string) (int64, error) { return 0, nil }); err != nil {
		t.Fatal(err)
	}
	job := plan.started(plan.due(base), context.Background(), base)
	plan.completed(job, base.Add(time.Second))
	monitor.IntervalSeconds = 60
	now := base.Add(2 * time.Minute)
	if err := plan.apply([]protocol.Monitor{monitor}, time.Second, now, func(string) (int64, error) { return 0, nil }); err != nil {
		t.Fatal(err)
	}
	state := plan.due(now)
	if state == nil || state.interval != time.Minute {
		t.Fatal("shortened interval did not make overdue target ready")
	}
	job = plan.started(state, context.Background(), now)
	if plan.due(now.Add(time.Minute)) != nil {
		t.Fatal("same target overlapped its pending check")
	}
	finished := now.Add(2 * time.Minute)
	plan.completed(job, finished)
	if plan.due(finished) != nil || !state.next.Equal(finished.Add(time.Minute)) {
		t.Fatal("slow target queued immediate catch-up checks")
	}
}

func TestConfigReplacementCancelsOldCheckAndPreservesTimeIdentity(t *testing.T) {
	base := time.Unix(1700000000, 0)
	plan := &schedule{}
	first := protocol.Monitor{ID: "web", Target: "https://old.invalid", IntervalSeconds: 60}
	if err := plan.apply([]protocol.Monitor{first}, time.Second, base, func(string) (int64, error) { return base.Unix() - 1, nil }); err != nil {
		t.Fatal(err)
	}
	job := plan.started(plan.due(base), context.Background(), base)
	replacement := first
	replacement.Target = "https://new.invalid"
	if err := plan.apply([]protocol.Monitor{replacement}, time.Second, base.Add(100*time.Millisecond), func(string) (int64, error) { return 0, nil }); err != nil {
		t.Fatal(err)
	}
	if job.ctx.Err() == nil || plan.due(base.Add(100*time.Millisecond)) != nil {
		t.Fatal("old target was not canceled or replacement overlapped")
	}
	plan.completed(job, base.Add(200*time.Millisecond))
	if plan.due(base.Add(200*time.Millisecond)) != nil {
		t.Fatal("replacement reused previous monitor Unix-second identity")
	}
	if state := plan.due(base.Add(time.Second)); state == nil || state.monitor.Target != replacement.Target {
		t.Fatal("replacement did not resume on the next real second")
	}
}

func schedulerAgent(t *testing.T, server string, concurrency int) *Agent {
	t.Helper()
	metrics, err := telemetry.New(context.Background(), false, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Server: server, Token: "test", DataDir: t.TempDir(), Interval: time.Second, FlushInterval: 5 * time.Minute, ConfigInterval: 5 * time.Minute, Concurrency: concurrency, MaxQueueBytes: 1 << 20, AllowInsecure: true}
	a, err := New(opts, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestSleepingSchedulerWakesForNewRemoteTarget(t *testing.T) {
	hits := make(chan string, 8)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- r.URL.Path
		w.WriteHeader(204)
	}))
	defer target.Close()
	a := schedulerAgent(t, target.URL, 2)
	a.config = protocol.Config{Version: 1, ProbeID: "sg", Monitors: []protocol.Monitor{{ID: "old", Target: target.URL + "/old", IntervalSeconds: 60}}}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- a.runChecks(ctx) }()
	defer func() {
		cancel()
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-hits:
	case <-time.After(time.Second):
		t.Fatal("initial target never checked")
	}
	a.mu.Lock()
	a.config.Monitors = append(a.config.Monitors, protocol.Monitor{ID: "new", Target: target.URL + "/new", IntervalSeconds: 300})
	a.mu.Unlock()
	a.notifyConfig()
	select {
	case path := <-hits:
		if path != "/new" {
			t.Fatalf("config change checked %s", path)
		}
	case <-time.After(time.Second):
		t.Fatal("sleeping 60-second deadline was not interrupted by config change")
	}
}

func TestWorkerPoolBoundsConcurrencyAndPersistsAllTargets(t *testing.T) {
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	var active, peak atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
		}
		started <- struct{}{}
		select {
		case <-release:
			w.WriteHeader(204)
		case <-r.Context().Done():
		}
	}))
	defer target.Close()
	a := schedulerAgent(t, target.URL, 2)
	a.config = protocol.Config{Version: 1, ProbeID: "sg", Monitors: []protocol.Monitor{{ID: "a", Target: target.URL, IntervalSeconds: 60}, {ID: "b", Target: target.URL, IntervalSeconds: 60}, {ID: "c", Target: target.URL, IntervalSeconds: 300}}}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- a.runChecks(ctx) }()
	defer func() {
		cancel()
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers not dispatched")
		}
	}
	select {
	case <-started:
		t.Fatal("third check exceeded concurrency2")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		count, _, err := a.queue.Stats()
		if err != nil {
			t.Fatal(err)
		}
		if count == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d targets persisted", count)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if peak.Load() > 2 {
		t.Fatalf("peak concurrency=%d", peak.Load())
	}
}

func TestOfflineCacheRetainsRemoteIntervalAndTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	a := schedulerAgent(t, server.URL, 1)
	cfg := protocol.Config{Version: 1, ProbeID: "sg", Monitors: []protocol.Monitor{{ID: "web", Target: "https://example.invalid", IntervalSeconds: 120, Timeout: 7300}}}
	if err := a.save(cfg); err != nil {
		t.Fatal(err)
	}
	opts := a.opts
	a.Close()
	reopened, err := New(opts, a.metrics, a.log)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.refresh(context.Background()); err == nil {
		t.Fatal("offline config fetch succeeded")
	}
	monitors := reopened.configuredMonitors()
	if len(monitors) != 1 || monitors[0].Timeout != 7300 || monitorInterval(monitors[0], time.Second) != 2*time.Minute {
		t.Fatalf("cached remote settings lost: %+v", monitors)
	}
}

func TestFlushCadenceIsCappedByRemoteIntervalsAndRefresh(t *testing.T) {
	a := &Agent{opts: Options{Interval: time.Second, FlushInterval: 5 * time.Minute}}
	a.config.Monitors = []protocol.Monitor{{ID: "slow", IntervalSeconds: 300}}
	if got := a.flushCadence(); got != 5*time.Minute {
		t.Fatalf("remote interval did not override CLI: %v", got)
	}
	a.config.Monitors = append(a.config.Monitors, protocol.Monitor{ID: "fast", IntervalSeconds: 60})
	if got := a.flushCadence(); got != time.Minute {
		t.Fatalf("new shorter remote interval = %v", got)
	}
	a.config.Monitors[1].IntervalSeconds = 0
	if got := a.flushCadence(); got != time.Second {
		t.Fatalf("legacy cached fallback = %v", got)
	}
}

func TestFirstScheduledResultUploadsPromptly(t *testing.T) {
	uploaded := make(chan protocol.Batch, 1)
	var targetURL string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/probes/config":
			_ = json.NewEncoder(w).Encode(protocol.Config{Version: 1, ProbeID: "sg", Monitors: []protocol.Monitor{{ID: "web", Target: targetURL, IntervalSeconds: 300, Timeout: 5000}}})
		case "/api/probes/ingest":
			var batch protocol.Batch
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
				t.Error(err)
				return
			}
			uploaded <- batch
			_ = json.NewEncoder(w).Encode(protocol.Ack{BatchID: batch.BatchID, Accepted: len(batch.Results)})
		default:
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	targetURL = server.URL + "/target"
	a := schedulerAgent(t, server.URL, 1)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- a.Run(ctx) }()
	select {
	case batch := <-uploaded:
		if len(batch.Results) != 1 || !batch.Results[0].Up {
			t.Fatalf("initial batch=%+v", batch)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-finished
		t.Fatal("first result waited for the 5-minute batch timer")
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
