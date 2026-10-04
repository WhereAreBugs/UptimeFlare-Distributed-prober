package check

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"testing"
)

func TestWindowsICMPStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		status uint32
		want   error
	}{{11010, context.DeadlineExceeded}, {5, errICMPPermission}, {11002, windows.WSAEHOSTUNREACH}, {11003, windows.WSAEHOSTUNREACH}, {11004, windows.WSAEHOSTUNREACH}, {11013, windows.WSAEHOSTUNREACH}} {
		if !errors.Is(windowsICMPError(tc.status), tc.want) {
			t.Fatalf("status%d classified incorrectly", tc.status)
		}
	}
}
