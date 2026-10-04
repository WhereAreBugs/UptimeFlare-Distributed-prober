package check

import (
	"context"
	"testing"

	"light-prober/internal/protocol"
)

func TestICMPLoopbackSockets(t *testing.T) {
	checker := New()
	defer checker.Close()
	for _, target := range []string{"127.0.0.1", "::1"} {
		t.Run(target, func(t *testing.T) {
			result := checker.Check(context.Background(), protocol.Monitor{Method: "ICMP_PING", Target: target, Timeout: 1000})
			if result.Stage == "configuration" && (result.Code == "permission" || result.Code == "unsupported") {
				t.Skip(result.Message)
			}
			if !result.Up || result.ICMPLatencyMS == nil || *result.ICMPLatencyMS < 0 || *result.ICMPLatencyMS > result.LatencyMS {
				t.Fatalf("real loopback ICMP=%+v", result)
			}
		})
	}
}

func TestICMPConfigurationAndCancellation(t *testing.T) {
	checker := New()
	defer checker.Close()
	for _, target := range []string{"https://example.com", "127.0.0.1:80", "user@host", "", "[::1]"} {
		assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Method: "ICMP_PING", Target: target}), "configuration", "target")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := checker.Check(ctx, protocol.Monitor{Method: "ICMP_PING", Target: "127.0.0.1"})
	if result.Stage == "configuration" && result.Code == "unsupported" {
		t.Skip(result.Message)
	}
	assertFailure(t, result, "icmp", "canceled")
}
