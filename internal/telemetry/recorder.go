package telemetry

import (
	"context"
	"net/http"
	"time"

	"light-prober/internal/protocol"
)

// Recorder keeps all SDK calls out of the disabled hot path.
type Recorder interface {
	Enabled() bool
	Start(context.Context, string) (context.Context, func(error))
	Inject(context.Context, http.Header)
	Operation(context.Context, string, time.Duration, error)
	Storage(int64, int64, int64)
	Active(int64)
	Identity(string)
	Check(context.Context, protocol.Result, string)
	Upload(context.Context, time.Duration, int, bool)
	Queue(uint64, int64)
	ConfigFailure(context.Context)
	Shutdown(context.Context) error
}

type noop struct{}

func (noop) Enabled() bool                                                      { return false }
func endNoop(error)                                                             {}
func (noop) Start(ctx context.Context, _ string) (context.Context, func(error)) { return ctx, endNoop }
func (noop) Inject(context.Context, http.Header)                                {}
func (noop) Operation(context.Context, string, time.Duration, error)            {}
func (noop) Storage(int64, int64, int64)                                        {}
func (noop) Active(int64)                                                       {}

func (noop) Identity(string) {}

func (noop) Check(context.Context, protocol.Result, string)   {}
func (noop) Upload(context.Context, time.Duration, int, bool) {}
func (noop) Queue(uint64, int64)                              {}
func (noop) ConfigFailure(context.Context)                    {}
func (noop) Shutdown(context.Context) error                   { return nil }
