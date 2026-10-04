package agent

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"light-prober/internal/protocol"
)

type scheduledMonitor struct {
	monitor          protocol.Monitor
	interval         time.Duration
	next, started    time.Time
	lastTime         int64
	generation       uint64
	desired, running bool
	cancel           context.CancelFunc
}

type schedule struct {
	monitors map[string]*scheduledMonitor
	running  int
}

func monitorInterval(monitor protocol.Monitor, fallback time.Duration) time.Duration {
	if monitor.IntervalSeconds != 0 {
		return time.Duration(monitor.IntervalSeconds) * time.Second
	}
	return fallback
}

func sameCheck(first, second protocol.Monitor) bool {
	first.IntervalSeconds, second.IntervalSeconds = 0, 0
	return reflect.DeepEqual(first, second)
}

// apply updates existing deadlines without restarting unaffected targets. Removed
// in-flight jobs remain tracked only until their cancellation completes.
func (s *schedule) apply(monitors []protocol.Monitor, fallback time.Duration, now time.Time, cursor func(string) (int64, error)) error {
	if s.monitors == nil {
		s.monitors = make(map[string]*scheduledMonitor, len(monitors))
	}
	for _, state := range s.monitors {
		state.desired = false
	}
	for _, monitor := range monitors {
		interval := monitorInterval(monitor, fallback)
		state := s.monitors[monitor.ID]
		if state == nil {
			lastTime, err := cursor(monitor.ID)
			if err != nil {
				return err
			}
			state = &scheduledMonitor{monitor: monitor, interval: interval, next: now, lastTime: lastTime, generation: 1}
			s.monitors[monitor.ID] = state
		} else {
			changed := !sameCheck(state.monitor, monitor)
			if changed {
				state.generation++
				state.next = now
				if state.cancel != nil {
					state.cancel()
				}
			} else if state.interval != interval && !state.started.IsZero() {
				state.next = state.started.Add(interval)
				if state.next.Before(now) {
					state.next = now
				}
			}
			state.monitor, state.interval = monitor, interval
		}
		state.desired = true
	}
	for id, state := range s.monitors {
		if state.desired {
			continue
		}
		if state.running {
			if state.cancel != nil {
				state.cancel()
			}
		} else {
			delete(s.monitors, id)
		}
	}
	return nil
}

// due selects one target without allocating a ready queue. At most 500 targets
// are scanned, and only the fixed worker count can be dispatched concurrently.
func (s *schedule) due(now time.Time) *scheduledMonitor {
	var chosen *scheduledMonitor
	for _, state := range s.monitors {
		if !state.desired || state.running || state.next.After(now) || now.Unix() <= state.lastTime {
			continue
		}
		if chosen == nil || state.next.Before(chosen.next) || state.next.Equal(chosen.next) && state.monitor.ID < chosen.monitor.ID {
			chosen = state
		}
	}
	return chosen
}

func (s *schedule) delay(now time.Time) (time.Duration, bool) {
	var shortest time.Duration
	found := false
	for _, state := range s.monitors {
		if !state.desired || state.running {
			continue
		}
		delay := state.next.Sub(now)
		if now.Unix() <= state.lastTime {
			// Check backward-clock recovery at most once per second. A same-second
			// restart waits exactly until the next real Unix-second boundary.
			clockDelay := time.Second
			if now.Unix() == state.lastTime {
				clockDelay -= time.Duration(now.Nanosecond())
			}
			delay = max(delay, clockDelay)
			if delay > time.Second && !state.next.After(now) {
				delay = time.Second
			}
		}
		delay = max(time.Duration(0), delay)
		if !found || delay < shortest {
			shortest, found = delay, true
		}
	}
	return shortest, found
}

type checkJob struct {
	state      *scheduledMonitor
	monitor    protocol.Monitor
	generation uint64
	stamp      int64
	ctx        context.Context
	cancel     context.CancelFunc
}

type checkOutcome struct {
	job checkJob
	err error
}

func (s *schedule) started(state *scheduledMonitor, ctx context.Context, now time.Time) checkJob {
	jobCtx, cancel := context.WithCancel(ctx)
	job := checkJob{state: state, monitor: state.monitor, generation: state.generation, stamp: now.Unix(), ctx: jobCtx, cancel: cancel}
	state.started, state.next = now, now.Add(state.interval)
	state.lastTime, state.running, state.cancel = job.stamp, true, cancel
	s.running++
	return job
}

func (s *schedule) completed(job checkJob, now time.Time) {
	state := job.state
	state.running, state.cancel = false, nil
	s.running--
	job.cancel()
	if !state.desired {
		delete(s.monitors, state.monitor.ID)
		return
	}
	if state.generation == job.generation && !state.next.After(now) {
		// A slow target never causes overlapping jobs or immediate catch-up loops.
		state.next = now.Add(state.interval)
	}
}

func (a *Agent) configuredMonitors() []protocol.Monitor {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.config.Monitors
}

func (a *Agent) perform(job checkJob) error {
	if job.ctx.Err() != nil {
		return nil
	}
	result := a.checker.Check(job.ctx, job.monitor)
	result.Time = job.stamp
	if job.ctx.Err() != nil {
		return nil
	}
	if err := a.queue.Append(result); err != nil {
		return fmt.Errorf("cannot persist result; checking stopped: %w", err)
	}
	a.metrics.Check(job.ctx, result, job.monitor.Method)
	a.firstResult.Do(func() { wake(a.resultReady) })
	return nil
}

func wake(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}

// runChecks owns one timer, a fixed worker pool and O(targets + workers) state.
// Config notifications interrupt sleeping deadlines immediately.
func (a *Agent) runChecks(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	jobs := make(chan checkJob, a.opts.Concurrency)
	done := make(chan checkOutcome, a.opts.Concurrency)
	var workers sync.WaitGroup
	for range a.opts.Concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-jobs:
					err := a.perform(job)
					job.cancel()
					select {
					case done <- checkOutcome{job: job, err: err}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	defer func() { cancel(); workers.Wait() }()
	plan := &schedule{}
	if err := plan.apply(a.configuredMonitors(), a.opts.Interval, time.Now(), a.queue.LastTimeFor); err != nil {
		return err
	}
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		for plan.running < a.opts.Concurrency {
			now := time.Now()
			state := plan.due(now)
			if state == nil {
				break
			}
			job := plan.started(state, ctx, now)
			select {
			case jobs <- job:
			case <-ctx.Done():
				job.cancel()
				return nil
			}
		}
		var deadline <-chan time.Time
		if plan.running < a.opts.Concurrency {
			if delay, active := plan.delay(time.Now()); active {
				timer.Reset(delay)
				deadline = timer.C
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-a.configChanged:
			if err := plan.apply(a.configuredMonitors(), a.opts.Interval, time.Now(), a.queue.LastTimeFor); err != nil {
				return err
			}
		case outcome := <-done:
			plan.completed(outcome.job, time.Now())
			a.observeQueue()
			if outcome.err != nil {
				return outcome.err
			}
		case <-deadline:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}
