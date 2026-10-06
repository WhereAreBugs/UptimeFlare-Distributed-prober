//go:build !linux && !darwin && !freebsd && !windows

package telemetry

func processUsage() (float64, float64, int64, bool) { return 0, 0, 0, false }
