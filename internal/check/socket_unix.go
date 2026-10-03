//go:build !windows

package check

import (
	"errors"
	"syscall"
)

func socketErrorCode(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "reset"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "unreachable"
	case errors.Is(err, syscall.ETIMEDOUT):
		return "timeout"
	default:
		return ""
	}
}
