//go:build windows

package telemetry

import "golang.org/x/sys/windows"

func processUsage() (user, system float64, peak int64, ok bool) {
	var created, exited, kernel, application windows.Filetime
	if windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &application) != nil {
		return
	}
	seconds := func(v windows.Filetime) float64 {
		return float64(uint64(v.HighDateTime)<<32|uint64(v.LowDateTime)) / 1e7
	}
	return seconds(application), seconds(kernel), 0, true
}
