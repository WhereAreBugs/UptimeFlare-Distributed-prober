package check

import (
	"errors"
	"syscall"
)

// Go's portable errno values on Windows are synthetic APPLICATION_ERROR values;
// real socket operations return Winsock's WSA* numeric codes instead.
const (
	wsaNetworkDown        = syscall.Errno(10050)
	wsaNetworkUnreachable = syscall.Errno(10051)
	wsaConnectionAborted  = syscall.Errno(10053)
	wsaConnectionReset    = syscall.Errno(10054)
	wsaTimedOut           = syscall.Errno(10060)
	wsaConnectionRefused  = syscall.Errno(10061)
	wsaHostUnreachable    = syscall.Errno(10065)
)

func socketErrorCode(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, wsaConnectionRefused):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE), errors.Is(err, wsaConnectionReset), errors.Is(err, wsaConnectionAborted):
		return "reset"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, wsaNetworkDown), errors.Is(err, wsaNetworkUnreachable), errors.Is(err, wsaHostUnreachable):
		return "unreachable"
	case errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, wsaTimedOut):
		return "timeout"
	default:
		return ""
	}
}
