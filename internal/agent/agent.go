// Package agent coordinates bounded checking, config refresh, and durable uploads.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"light-prober/internal/check"
	"light-prober/internal/client"
	"light-prober/internal/protocol"
	"light-prober/internal/spool"
	"light-prober/internal/telemetry"
)

type Options struct {
	Server, Token, DataDir                  string
	Version, WebListen, WebPassword         string
	Interval, FlushInterval, ConfigInterval time.Duration
	Concurrency                             int
	MaxQueueBytes                           int64
	Compress, AllowInsecure                 bool
}

type Agent struct {
	opts                                     Options
	client                                   *client.Client
	queue                                    *spool.Queue
	checker                                  *check.Checker
	metrics                                  telemetry.Recorder
	log                                      *slog.Logger
	mu                                       sync.RWMutex
	config                                   protocol.Config
	identity                                 string
	configChanged                            chan struct{}
	uploadChanged                            chan struct{}
	resultReady                              chan struct{}
	firstResult                              sync.Once
	startedAt                                time.Time
	lastConfigAt, lastUploadAt, nextUploadAt int64
	uploadState                              string
	checkingPaused                           bool
}

type cache struct {
	Server    string          `json:"server"`
	Config    protocol.Config `json:"config"`
	UpdatedAt int64           `json:"updated_at,omitempty"`
}

type fatalError struct{ error }

func New(opts Options, metrics telemetry.Recorder, log *slog.Logger) (*Agent, error) {
	if opts.Interval < time.Second || opts.FlushInterval < time.Second || opts.ConfigInterval < time.Second || opts.Concurrency < 1 || opts.Concurrency > 32 || opts.MaxQueueBytes < 1<<20 {
		return nil, errors.New("intervals must be >=1s, concurrency 1..32, queue >=1 MiB")
	}
	c, err := client.New(opts.Server, opts.Token, opts.Compress, opts.AllowInsecure)
	if err != nil {
		return nil, err
	}
	c.SetTelemetry(metrics)
	q, err := spool.Open(filepath.Join(opts.DataDir, "queue.db"), opts.MaxQueueBytes)
	if err != nil {
		c.Close()
		return nil, err
	}
	a := &Agent{opts: opts, client: c, queue: q, checker: check.New(), metrics: metrics, log: log,
		startedAt: time.Now(), uploadState: "idle",
		configChanged: make(chan struct{}, 1), uploadChanged: make(chan struct{}, 1), resultReady: make(chan struct{}, 1)}
	if err = a.load(); err != nil {
		a.Close()
		return nil, err
	}
	a.metrics.Identity(a.identity)
	a.observeQueue()
	return a, nil
}

func (a *Agent) Close() { a.client.Close(); a.checker.Close(); _ = a.queue.Close() }

func (a *Agent) load() error {
	file, err := os.Open(filepath.Join(a.opts.DataDir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		n, _, e := a.queue.Stats()
		if e != nil {
			return e
		}
		if n > 0 {
			return errors.New("queue exists without identity cache; restore config.json before reuse")
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("oversized config cache")
	}
	var value cache
	if json.Unmarshal(data, &value) != nil || client.ValidateConfig(value.Config) != nil {
		return errors.New("invalid config cache; preserve queue.db and restore configuration")
	}
	if value.Server != strings.TrimRight(a.opts.Server, "/") {
		return errors.New("data directory belongs to another receiver; select a new --data-dir")
	}
	a.config = value.Config
	a.identity = value.Config.ProbeID
	a.lastConfigAt = value.UpdatedAt
	return nil
}

func (a *Agent) save(cfg protocol.Config) error {
	data, err := json.Marshal(cache{Server: strings.TrimRight(a.opts.Server, "/"), Config: cfg, UpdatedAt: time.Now().Unix()})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(a.opts.DataDir, ".config-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(name, filepath.Join(a.opts.DataDir, "config.json")); err != nil {
		return err
	}
	return syncConfigDirectory(a.opts.DataDir)
}

func (a *Agent) refresh(ctx context.Context) (refreshErr error) {
	ctx, end := a.metrics.Start(ctx, "config.refresh")
	start := time.Now()
	defer func() { a.metrics.Operation(ctx, "config.refresh", time.Since(start), refreshErr); end(refreshErr) }()
	cfg, err := a.client.Config(ctx)
	if err != nil {
		a.metrics.ConfigFailure(ctx)
		var httpErr *client.HTTPError
		if errors.As(err, &httpErr) && (httpErr.Status == 401 || httpErr.Status == 403) {
			a.mu.Lock()
			a.config.Monitors = nil
			a.mu.Unlock()
			a.notifyConfig()
		}
		return err
	}
	if a.identity != "" && cfg.ProbeID != a.identity {
		return fatalError{errors.New("probe identity changed; select a new data directory to preserve the original queue")}
	}
	if err = a.save(cfg); err != nil {
		return fatalError{fmt.Errorf("persist config: %w", err)}
	}
	a.mu.RLock()
	changed := !reflect.DeepEqual(a.config, cfg)
	a.mu.RUnlock()
	ids := make([]string, 0, len(cfg.Monitors)+len(cfg.DisplayMonitors))
	for _, m := range cfg.Monitors {
		ids = append(ids, m.ID)
	}
	for _, m := range cfg.DisplayMonitors {
		ids = append(ids, m.ID)
	}
	if changed {
		if err = a.queue.PruneHistory(ids); err != nil {
			return fatalError{err}
		}
	}
	a.mu.Lock()
	a.config = cfg
	a.identity = cfg.ProbeID
	a.lastConfigAt = time.Now().Unix()
	a.mu.Unlock()
	a.metrics.Identity(cfg.ProbeID)
	if changed {
		a.notifyConfig()
	}
	return nil
}

func (a *Agent) notifyConfig() {
	wake(a.configChanged)
	wake(a.uploadChanged)
}

func (a *Agent) observeQueue() {
	if !a.metrics.Enabled() {
		return
	}
	snapshot, err := a.queue.Snapshot()
	if err == nil {
		a.metrics.Queue(snapshot.Count, snapshot.Bytes)
		a.metrics.Storage(snapshot.DatabaseBytes, snapshot.DatabaseLimit, snapshot.OldestAt)
	}
}

func (a *Agent) round(ctx context.Context) error {
	a.mu.RLock()
	monitors := a.config.Monitors
	a.mu.RUnlock()
	if len(monitors) == 0 {
		return nil
	}
	workers := min(a.opts.Concurrency, len(monitors))
	// A round has a stable timestamp even when monitors wait for a worker slot.
	// The persistent cursor also prevents identity reuse across fast restarts or
	// backward clock jumps. Wait for real wall time; never fabricate future samples.
	lastTime, err := a.queue.LastTime()
	if err != nil {
		return fmt.Errorf("read persisted sample time: %w", err)
	}
	var roundTime int64
	for {
		if ctx.Err() != nil {
			return nil
		}
		now := time.Now()
		roundTime = now.Unix()
		if roundTime > lastTime {
			break
		}
		// Recheck at least once a second so clock corrections are noticed promptly.
		delay := time.Second
		if roundTime == lastTime {
			delay -= time.Duration(now.Nanosecond())
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	jobs := make(chan protocol.Monitor)
	errCh := make(chan error, 1)
	checkCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for monitor := range jobs {
				if checkCtx.Err() != nil {
					return
				}
				result := a.checker.Check(checkCtx, monitor)
				result.Time = roundTime
				if checkCtx.Err() != nil {
					return
				}
				if err := a.queue.Append(result); err != nil {
					select {
					case errCh <- fmt.Errorf("cannot persist result; checking stopped: %w", err):
					default:
					}
					cancel()
					return
				}
				a.metrics.Check(ctx, result, monitor.Method)
			}
		}()
	}
send:
	for _, m := range monitors {
		select {
		case jobs <- m:
		case <-checkCtx.Done():
			break send
		}
	}
	close(jobs)
	wg.Wait()
	a.observeQueue()
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

// flush bounds each recovery pass to ten batches so a large backlog yields between passes.
func (a *Agent) flush(ctx context.Context) (bool, error) {
	for i := 0; i < 10; i++ {
		batchCtx, end := a.metrics.Start(ctx, "upload.batch")
		startPeek := time.Now()
		entries, err := a.queue.Peek(protocol.MaxBatchResults)
		a.metrics.Operation(batchCtx, "queue.peek", time.Since(startPeek), err)
		if err != nil {
			end(err)
			return false, err
		}
		if len(entries) == 0 {
			end(nil)
			return false, nil
		}
		results := make([]protocol.Result, len(entries))
		for i, e := range entries {
			results[i] = e.Result
		}
		start := time.Now()
		wire, err := a.client.Upload(batchCtx, results)
		a.metrics.Upload(batchCtx, time.Since(start), wire, err == nil)
		if err != nil {
			end(err)
			return true, err
		}
		ackCtx, ackEnd := a.metrics.Start(batchCtx, "queue.ack")
		ackStart := time.Now()
		err = a.queue.Ack(entries)
		a.metrics.Operation(ackCtx, "queue.ack", time.Since(ackStart), err)
		ackEnd(err)
		end(err)
		if err != nil {
			return true, fmt.Errorf("persist acknowledgement: %w", err)
		}
		a.mu.Lock()
		a.lastUploadAt = time.Now().Unix()
		a.mu.Unlock()
		a.observeQueue()
	}
	n, _, err := a.queue.Stats()
	return n > 0, err
}

func (a *Agent) uploadLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	backoff := 15 * time.Second
	retrying := false
	lastFlush, nextAt := time.Now(), time.Now()
	reset := func(at time.Time) {
		a.mu.Lock()
		a.nextUploadAt = at.Unix()
		a.mu.Unlock()
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		nextAt = at
		timer.Reset(max(time.Duration(0), time.Until(at)))
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.resultReady:
			if !retrying {
				reset(time.Now())
			}
		case <-a.uploadChanged:
			if !retrying {
				// A shorter target cadence must not leave results waiting behind
				// a timer calculated from the previous, slower configuration.
				at := lastFlush.Add(a.flushCadence())
				if at.Before(nextAt) {
					reset(at)
				}
			}
		case <-timer.C:
			a.mu.Lock()
			a.uploadState = "uploading"
			a.mu.Unlock()
			more, err := a.flush(ctx)
			a.mu.Lock()
			a.uploadState = "idle"
			if err != nil {
				a.uploadState = "retrying"
			}
			a.mu.Unlock()
			lastFlush = time.Now()
			next := a.flushCadence()
			retrying = err != nil
			if err != nil && ctx.Err() == nil {
				a.log.Warn("batch upload failed; queue retained", "error", err)
				next = backoff + time.Duration(rand.Int64N(int64(backoff/4)+1))
				backoff = min(backoff*2, 5*time.Minute)
				var httpErr *client.HTTPError
				if errors.As(err, &httpErr) && httpErr.RetryAfter > next {
					next = httpErr.RetryAfter
				}
			} else {
				backoff = 15 * time.Second
				if more {
					next = time.Second
				}
			}
			reset(lastFlush.Add(next))
		}
	}
}

func (a *Agent) flushCadence() time.Duration {
	interval := a.opts.FlushInterval
	for _, monitor := range a.configuredMonitors() {
		interval = min(interval, monitorInterval(monitor, a.opts.Interval))
	}
	return interval
}

func (a *Agent) Run(ctx context.Context) error {
	if err := a.refresh(ctx); err != nil {
		var fatal fatalError
		if errors.As(err, &fatal) {
			return err
		}
		a.log.Warn("configuration unavailable; using cached monitors if present", "error", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	web, err := a.startWeb(runCtx)
	if err != nil {
		return err
	}
	if web != nil {
		defer web.Close()
	}
	var bg sync.WaitGroup
	fatalCh := make(chan error, 1)
	bg.Add(2)
	go func() { defer bg.Done(); a.uploadLoop(runCtx) }()
	go func() {
		defer bg.Done()
		ticker := time.NewTicker(a.opts.ConfigInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if err := a.refresh(runCtx); err != nil && runCtx.Err() == nil {
					var fatal fatalError
					if errors.As(err, &fatal) {
						fatalCh <- err
						cancel()
						return
					}
					a.log.Warn("configuration refresh failed", "error", err)
				}
			}
		}
	}()
	defer func() {
		cancel()
		bg.Wait()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = a.flush(shutdown)
	}()
	if err := a.runChecksWithCapacity(runCtx); err != nil {
		return err
	}
	select {
	case err := <-fatalCh:
		return err
	default:
		return nil
	}
}

// A full outbox pauses collection while keeping uploads and the dashboard
// running. Resume only after enough space for a maximum-sized result exists.
func (a *Agent) runChecksWithCapacity(ctx context.Context) error {
	for {
		err := a.runChecks(ctx)
		if !errors.Is(err, spool.ErrFull) {
			return err
		}
		a.mu.Lock()
		a.checkingPaused = true
		a.mu.Unlock()
		a.log.Warn("database capacity reached; checking paused, existing queue retained")
		for {
			timer := time.NewTimer(30 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			ready, err := a.queue.CanResume()
			if err != nil {
				return err
			}
			if ready {
				break
			}
		}
		a.mu.Lock()
		a.checkingPaused = false
		a.mu.Unlock()
	}
}
