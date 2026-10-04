//go:build linux || darwin

package check

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

func nativeICMP(ctx context.Context, target *net.IPAddr) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	network, local, protocol := "udp4", "0.0.0.0", 1
	var requestType, replyType icmp.Type = ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply
	if target.IP.To4() == nil {
		network, local, protocol = "udp6", "::", 58
		requestType, replyType = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply
	}
	connection, err := icmp.ListenPacket(network, local)
	if errors.Is(err, os.ErrPermission) {
		return 0, errICMPPermission
	}
	if err != nil {
		return 0, err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, err
	}
	message := icmp.Message{Type: requestType, Code: 0, Body: &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: 1, Data: nonce[:]}}
	packet, err := message.Marshal(nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	if _, err := connection.WriteTo(packet, &net.UDPAddr{IP: target.IP, Zone: target.Zone}); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	var buffer [512]byte
	for {
		length, peer, err := connection.ReadFrom(buffer[:])
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, err
		}
		response, err := icmp.ParseMessage(protocol, buffer[:length])
		if err != nil {
			continue
		}
		if quotedICMPFailure(response, protocol, requestType, nonce[:]) {
			return 0, errICMPUnreachable
		}
		if response.Type != replyType || response.Code != 0 {
			continue
		}
		echo, ok := response.Body.(*icmp.Echo)
		if !ok || echo.Seq != 1 || !bytes.Equal(echo.Data, nonce[:]) {
			continue
		}
		peerIP := peer.String()
		if udp, ok := peer.(*net.UDPAddr); ok {
			peerIP = udp.IP.String()
		}
		if ip, ok := peer.(*net.IPAddr); ok {
			peerIP = ip.IP.String()
		}
		if !target.IP.Equal(net.ParseIP(peerIP)) {
			continue
		}
		return time.Since(start), nil
	}
}

// ICMP errors quote the original IP packet. Only attribute one to this check if
// the quoted echo carries our entire unpredictable nonce; short/noisy quotes
// remain ignored rather than reporting another socket's failure.
func quotedICMPFailure(response *icmp.Message, protocol int, requestType icmp.Type, nonce []byte) bool {
	var quoted []byte
	switch body := response.Body.(type) {
	case *icmp.DstUnreach:
		quoted = body.Data
	case *icmp.TimeExceeded:
		quoted = body.Data
	default:
		return false
	}
	header := 40
	if protocol == 1 {
		if len(quoted) < 20 || quoted[0]>>4 != 4 {
			return false
		}
		header = int(quoted[0]&15) * 4
		if header < 20 || quoted[9] != 1 {
			return false
		}
	} else if len(quoted) < 40 || quoted[0]>>4 != 6 || quoted[6] != 58 {
		return false
	}
	if len(quoted) < header+8+len(nonce) {
		return false
	}
	request, err := icmp.ParseMessage(protocol, quoted[header:])
	if err != nil || request.Type != requestType || request.Code != 0 {
		return false
	}
	echo, ok := request.Body.(*icmp.Echo)
	return ok && echo.Seq == 1 && bytes.Equal(echo.Data, nonce)
}
