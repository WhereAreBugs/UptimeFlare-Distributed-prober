package check

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"light-prober/internal/protocol"
)

var errICMPUnsupported = errors.New("native ICMP is unsupported on this platform")
var errICMPPermission = errors.New("native ICMP permission denied")
var errICMPUnreachable = errors.New("ICMP destination is unreachable")

func (c *Checker) checkICMP(ctx context.Context, monitor protocol.Monitor, result protocol.Result) protocol.Result {
	if monitor.Target == "" || strings.ContainsAny(monitor.Target, "/\\ \t\r\n?#@") {
		return failed(result, "configuration", "target", "ICMP target must be a hostname or IP address without a port")
	}
	var address *net.IPAddr
	if ip, err := netip.ParseAddr(monitor.Target); err == nil {
		address = &net.IPAddr{IP: net.IP(ip.AsSlice()), Zone: ip.Zone()}
	} else {
		if strings.Contains(monitor.Target, ":") {
			return failed(result, "configuration", "target", "ICMP target must be a hostname or IP address without a port")
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, monitor.Target)
		if err != nil {
			return failureFor(result, "dns", err)
		}
		if len(addresses) == 0 {
			return failed(result, "dns", "not_found", "DNS name did not resolve to an address")
		}
		address = &addresses[0]
		for i := range addresses {
			if addresses[i].IP.To4() != nil {
				address = &addresses[i]
				break
			}
		}
	}
	rtt, err := nativeICMP(ctx, address)
	if errors.Is(err, errICMPUnreachable) {
		return failed(result, "icmp", "unreachable", "ICMP destination is unreachable")
	}
	if errors.Is(err, errICMPPermission) {
		return failed(result, "configuration", "permission", "ICMP socket permission denied; configure ping socket access or an authenticated proxy")
	}
	if errors.Is(err, errICMPUnsupported) {
		return failed(result, "configuration", "unsupported", "Native ICMP is unavailable; configure an authenticated ICMP proxy")
	}
	if err != nil {
		return failureFor(result, "icmp", err)
	}
	latency := float64(rtt) / float64(time.Millisecond)
	result.ICMPLatencyMS, result.Up = &latency, true
	return result
}
