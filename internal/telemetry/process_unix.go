//go:build linux || darwin || freebsd

package telemetry

import (
	"golang.org/x/sys/unix"
	"runtime"
)

func processUsage() (user, system float64, peak int64, ok bool) {
	var v unix.Rusage
	if unix.Getrusage(unix.RUSAGE_SELF, &v) != nil {
		return
	}
	user = float64(v.Utime.Sec) + float64(v.Utime.Usec)/1e6
	system = float64(v.Stime.Sec) + float64(v.Stime.Usec)/1e6
	peak = int64(v.Maxrss)
	if runtime.GOOS != "darwin" {
		peak *= 1024
	}
	return user, system, peak, true
}
