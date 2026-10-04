package check

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"runtime"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ipHelper        = windows.NewLazySystemDLL("iphlpapi.dll")
	icmpCreateFile  = ipHelper.NewProc("IcmpCreateFile")
	icmp6CreateFile = ipHelper.NewProc("Icmp6CreateFile")
	icmpCloseHandle = ipHelper.NewProc("IcmpCloseHandle")
	icmpSendEcho    = ipHelper.NewProc("IcmpSendEcho")
	icmp6SendEcho2  = ipHelper.NewProc("Icmp6SendEcho2")
)

// Windows IP Helper sends echo without raw-socket/admin privileges. Synchronous
// calls are bounded by the configured deadline; cancellation is checked before
// and after the native call, without spawning an unbounded background goroutine.
func nativeICMP(ctx context.Context, target *net.IPAddr) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	timeout := defaultTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		return 0, context.DeadlineExceeded
	}
	create := icmpCreateFile
	if target.IP.To4() == nil {
		create = icmp6CreateFile
	}
	if err := create.Find(); err != nil {
		return 0, errICMPUnsupported
	}
	send := icmpSendEcho
	if target.IP.To4() == nil {
		send = icmp6SendEcho2
	}
	if send.Find() != nil || icmpCloseHandle.Find() != nil {
		return 0, errICMPUnsupported
	}
	handle, _, err := create.Call()
	if handle == ^uintptr(0) || handle == 0 {
		if err == windows.ERROR_ACCESS_DENIED {
			return 0, errICMPPermission
		}
		return 0, err
	}
	defer icmpCloseHandle.Call(handle)
	var request [16]byte
	var reply [512]byte
	var count uintptr
	milliseconds := uintptr(max(int64(1), (timeout.Milliseconds() + 1)))
	if ip := target.IP.To4(); ip != nil {
		count, _, err = icmpSendEcho.Call(handle, uintptr(binary.LittleEndian.Uint32(ip)), uintptr(unsafe.Pointer(&request[0])), uintptr(len(request)), 0, uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), milliseconds)
	} else {
		source := windows.RawSockaddrInet6{Family: windows.AF_INET6}
		destination := windows.RawSockaddrInet6{Family: windows.AF_INET6}
		copy(destination.Addr[:], target.IP.To16())
		if target.Zone != "" {
			index, zoneErr := strconv.ParseUint(target.Zone, 10, 32)
			if zoneErr != nil {
				iface, err := net.InterfaceByName(target.Zone)
				if err != nil {
					return 0, err
				}
				index = uint64(iface.Index)
			}
			destination.Scope_id = uint32(index)
		}
		count, _, err = icmp6SendEcho2.Call(handle, 0, 0, 0, uintptr(unsafe.Pointer(&source)), uintptr(unsafe.Pointer(&destination)), uintptr(unsafe.Pointer(&request[0])), uintptr(len(request)), 0, uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), milliseconds)
		runtime.KeepAlive(source)
		runtime.KeepAlive(destination)
	}
	runtime.KeepAlive(request)
	runtime.KeepAlive(reply)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if count == 0 {
		var errno windows.Errno
		if errors.As(err, &errno) && errno != 0 {
			return 0, windowsICMPError(uint32(errno))
		}
		return 0, errICMPUnsupported
	}
	statusOffset := 4
	if target.IP.To4() == nil {
		statusOffset = 28
	}
	status := binary.LittleEndian.Uint32(reply[statusOffset : statusOffset+4])
	if status != 0 {
		return 0, windowsICMPError(status)
	}
	rtt := binary.LittleEndian.Uint32(reply[statusOffset+4 : statusOffset+8])
	return time.Duration(rtt) * time.Millisecond, nil
}

func windowsICMPError(status uint32) error {
	switch status {
	case 11010:
		return context.DeadlineExceeded // IP_REQ_TIMED_OUT
	case 5:
		return errICMPPermission
	case 11002, 11003, 11004, 11005, 11013:
		return windows.WSAEHOSTUNREACH
	}
	return windows.Errno(status)
}
