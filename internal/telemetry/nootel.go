//go:build nootel

package telemetry

import (
	"context"
	"errors"
	"time"
)

func New(_ context.Context, enabled bool, _ time.Duration) (Recorder, error) {
	if enabled {
		return nil, errors.New("this binary was built with nootel; use the standard binary for telemetry")
	}
	return noop{}, nil
}
