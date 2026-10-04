//go:build linux || darwin

package check

import (
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"testing"
)

func TestQuotedICMPFailuresAreCorrelatedToThisCheck(t *testing.T) {
	nonce := []byte("random-nonce-1234")
	packet, err := (&icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: 1, Seq: 1, Data: nonce}}).Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 20)
	header[0] = 0x45
	header[9] = 1
	quote := append(header, packet...)
	message := &icmp.Message{Type: ipv4.ICMPTypeDestinationUnreachable, Body: &icmp.DstUnreach{Data: quote}}
	if !quotedICMPFailure(message, 1, ipv4.ICMPTypeEcho, nonce) {
		t.Fatal("own unreachable error ignored")
	}
	if quotedICMPFailure(message, 1, ipv4.ICMPTypeEcho, []byte("other-nonce-12345")) {
		t.Fatal("foreign echo attributed to this check")
	}
	message.Body = &icmp.DstUnreach{Data: quote[:28]}
	if quotedICMPFailure(message, 1, ipv4.ICMPTypeEcho, nonce) {
		t.Fatal("uncorrelated truncated quote accepted")
	}
}
