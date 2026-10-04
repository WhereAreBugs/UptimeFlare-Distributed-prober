//go:build !linux && !darwin && !windows

package check

import (
	"context"
	"net"
	"time"
)

func nativeICMP(context.Context, *net.IPAddr) (time.Duration, error) { return 0, errICMPUnsupported }
