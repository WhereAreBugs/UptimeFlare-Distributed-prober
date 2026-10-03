package check

import (
	"net"
	"os"
	"syscall"
	"testing"
)

func TestWrappedWinsockFailures(t *testing.T) {
	for _, fixture := range []struct {
		err  syscall.Errno
		code string
	}{{wsaConnectionRefused, "refused"}, {wsaConnectionReset, "reset"}, {wsaConnectionAborted, "reset"}, {wsaNetworkUnreachable, "unreachable"}, {wsaHostUnreachable, "unreachable"}, {wsaTimedOut, "timeout"}} {
		err := &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: fixture.err}}
		if got := socketErrorCode(err); got != fixture.code {
			t.Fatalf("Winsock %d = %q, want %q", fixture.err, got, fixture.code)
		}
	}
}
