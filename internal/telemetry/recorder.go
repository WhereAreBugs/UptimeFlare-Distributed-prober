package telemetry

import (
	"context"
	"time"

	"light-prober/internal/protocol"
)

// Recorder keeps all SDK calls out of the disabled hot path.
type Recorder interface {
	Identity(string)
	Check(context.Context, protocol.Result)
	Upload(context.Context, time.Duration, int, bool)
	Queue(uint64, int64)
	ConfigFailure(context.Context)
	Shutdown(context.Context) error
}

type noop struct{}

func (noop) Identity(string) {}

func (noop) Check(context.Context, protocol.Result)           {}
func (noop) Upload(context.Context, time.Duration, int, bool) {}
func (noop) Queue(uint64, int64)                              {}
func (noop) ConfigFailure(context.Context)                    {}
func (noop) Shutdown(context.Context) error                   { return nil }
