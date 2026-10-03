// Package check performs resource-bounded network checks and reports the stage
// at which a failure occurred. Error messages never include target URLs,
// request headers, response bodies, or credentials.
package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"light-prober/internal/protocol"
)

const (
	defaultTimeout = 10 * time.Second
	maxKeywordBody = 1 << 20
	maxKeywordSize = 4096
	maxDrainBody   = 32 << 10
)

// Checker reuses a bounded HTTP connection pool and is safe for concurrent use.
// The caller bounds the number of concurrently executing checks.
type Checker struct {
	client    *http.Client
	transport *http.Transport
	dial      func(context.Context, string, string) (net.Conn, error)
}

func New() *Checker {
	dialer := &net.Dialer{KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                  http.ProxyFromEnvironment,
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           32,
		MaxIdleConnsPerHost:    2,
		MaxConnsPerHost:        4,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableCompression:     true,
	}
	return &Checker{
		client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		transport: transport,
		dial:      dialer.DialContext,
	}
}

func (c *Checker) Close() { c.transport.CloseIdleConnections() }

func (c *Checker) Check(parent context.Context, monitor protocol.Monitor) (result protocol.Result) {
	start := time.Now()
	result = protocol.Result{MonitorID: monitor.ID, Time: start.Unix()}
	defer func() { result.LatencyMS = float64(time.Since(start)) / float64(time.Millisecond) }()
	timeout := defaultTimeout
	if monitor.Timeout < 0 || int64(monitor.Timeout) > int64((1<<63-1)/time.Millisecond) {
		return failed(result, "configuration", "timeout", "Check timeout must be positive")
	}
	if monitor.Timeout > 0 {
		timeout = time.Duration(monitor.Timeout) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	method := strings.ToUpper(monitor.Method)
	if method == "TCP_PING" {
		return c.checkTCP(ctx, monitor, result)
	}
	if method == "" {
		method = http.MethodGet
	}
	if !validMethod(method) {
		return failed(result, "configuration", "method", "HTTP method is invalid")
	}
	if len(monitor.ResponseKeyword) > maxKeywordSize || len(monitor.ResponseForbiddenKeyword) > maxKeywordSize {
		return failed(result, "configuration", "keyword", "Response keyword exceeds the bounded scan")
	}
	u, err := url.Parse(monitor.Target)
	if err != nil || u == nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return failed(result, "configuration", "target", "An HTTP or HTTPS URL is required")
	}
	if u.User != nil {
		return failed(result, "configuration", "target", "URL credentials are unsupported; configure an authorization header")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(monitor.Body))
	if err != nil {
		return failed(result, "configuration", "request", "HTTP request configuration is invalid")
	}
	for key, value := range monitor.Headers {
		if !validHeaderName(key) || strings.ContainsAny(value, "\r\n\x00") {
			return failed(result, "configuration", "header", "HTTP header configuration is invalid")
		}
		if strings.EqualFold(key, "Host") {
			req.Host = value
		} else {
			req.Header.Set(key, value)
		}
	}
	state := &traceState{stage: "tcp"}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), state.trace()))
	resp, err := c.client.Do(req)
	if err != nil {
		return failureFor(result, state.current(), err)
	}
	defer resp.Body.Close()
	if !acceptedStatus(resp.StatusCode, monitor.ExpectedCodes) {
		return failed(result, "http", "status", "Unexpected HTTP response status "+strconv.Itoa(resp.StatusCode))
	}
	if monitor.ResponseKeyword == "" && monitor.ResponseForbiddenKeyword == "" {
		// Bounded draining enables keep-alive for small responses without buffering
		// or reading an unbounded stream. Large bodies are closed immediately.
		_, err = io.CopyN(io.Discard, resp.Body, maxDrainBody)
		if err != nil && !errors.Is(err, io.EOF) {
			return failureFor(result, "body", err)
		}
		result.Up = true
		return result
	}
	if resp.ContentLength >= maxKeywordBody {
		return failed(result, "body", "too_large", "Response exceeds the bounded keyword scan")
	}
	wanted, forbidden, exhausted, err := scanKeywords(resp.Body, monitor.ResponseKeyword, monitor.ResponseForbiddenKeyword)
	if err != nil {
		return failureFor(result, "body", err)
	}
	if exhausted {
		return failed(result, "body", "too_large", "Response exceeds the bounded keyword scan")
	}
	if monitor.ResponseKeyword != "" && !wanted {
		return failed(result, "body", "keyword", "Required response keyword is absent")
	}
	if monitor.ResponseForbiddenKeyword != "" && forbidden {
		return failed(result, "body", "keyword", "Forbidden response keyword is present")
	}
	result.Up = true
	return result
}

var scanBuffers = sync.Pool{New: func() any { return make([]byte, 32<<10) }}

// scanKeywords uses streaming KMP matchers, retaining at most a 32 KiB pooled
// read buffer plus the keyword failure tables. Matches crossing read boundaries
// are preserved. The response is never buffered in its entirety.
func scanKeywords(reader io.Reader, wanted, forbidden string) (bool, bool, bool, error) {
	first, second := newMatcher(wanted), newMatcher(forbidden)
	buffer := scanBuffers.Get().([]byte)
	defer func() {
		clear(buffer)
		scanBuffers.Put(buffer)
	}()
	read, emptyReads := 0, 0
	for read < maxKeywordBody {
		limit := min(len(buffer), maxKeywordBody-read)
		n, err := reader.Read(buffer[:limit])
		read += n
		first.consume(buffer[:n])
		second.consume(buffer[:n])
		if err != nil {
			if errors.Is(err, io.EOF) {
				return first.found, second.found, false, nil
			}
			return first.found, second.found, false, err
		}
		if n == 0 {
			emptyReads++
			if emptyReads >= 100 {
				return first.found, second.found, false, io.ErrNoProgress
			}
		} else {
			emptyReads = 0
		}
	}
	return first.found, second.found, true, nil
}

type matcher struct {
	pattern string
	failure []int
	matched int
	found   bool
}

func newMatcher(pattern string) matcher {
	m := matcher{pattern: pattern}
	if pattern == "" {
		return m
	}
	m.failure = make([]int, len(pattern))
	for i, previous := 1, 0; i < len(pattern); i++ {
		for previous > 0 && pattern[i] != pattern[previous] {
			previous = m.failure[previous-1]
		}
		if pattern[i] == pattern[previous] {
			previous++
		}
		m.failure[i] = previous
	}
	return m
}

func (m *matcher) consume(data []byte) {
	if m.pattern == "" || m.found {
		return
	}
	for _, char := range data {
		for m.matched > 0 && char != m.pattern[m.matched] {
			m.matched = m.failure[m.matched-1]
		}
		if char == m.pattern[m.matched] {
			m.matched++
		}
		if m.matched == len(m.pattern) {
			m.found = true
			return
		}
	}
}

func (c *Checker) checkTCP(ctx context.Context, monitor protocol.Monitor, result protocol.Result) protocol.Result {
	host, port, err := net.SplitHostPort(monitor.Target)
	number, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || number < 1 || number > 65535 {
		return failed(result, "configuration", "target", "TCP target must be host:port with a numeric port")
	}
	conn, err := c.dial(ctx, "tcp", monitor.Target)
	if err != nil {
		return failureFor(result, "tcp", err)
	}
	_ = conn.Close()
	result.Up = true
	return result
}

func acceptedStatus(status int, expected []int) bool {
	if len(expected) == 0 {
		return status >= 200 && status < 300
	}
	for _, code := range expected {
		if status == code {
			return true
		}
	}
	return false
}

func validMethod(method string) bool { return validHeaderName(method) }

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", char) {
			continue
		}
		return false
	}
	return true
}

func failed(result protocol.Result, stage, code, message string) protocol.Result {
	result.Stage, result.Code, result.Message = stage, code, message
	return result
}

func failureFor(result protocol.Result, stage string, err error) protocol.Result {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		stage = "dns"
		if dns.IsNotFound {
			return failed(result, stage, "not_found", "DNS name was not found")
		}
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalidCertificate) || errors.As(err, &verification) {
		return failed(result, "tls", "certificate", "TLS certificate verification failed")
	}
	if errors.Is(err, context.Canceled) {
		return failed(result, stage, "canceled", "Check was canceled")
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return failed(result, stage, "timeout", "Check deadline exceeded")
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return failed(result, stage, "refused", "Connection was refused")
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return failed(result, stage, "reset", "Connection was reset")
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) {
		return failed(result, stage, "unreachable", "Network or host is unreachable")
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return failed(result, stage, "closed", "Connection closed before the response completed")
	}
	return failed(result, stage, "unknown", "Check failed during "+stage)
}

type traceState struct {
	mu     sync.Mutex
	stage  string
	reused bool
}

func (s *traceState) set(stage string) {
	s.mu.Lock()
	s.stage = stage
	s.mu.Unlock()
}

func (s *traceState) current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage
}

func (s *traceState) connecting(stage string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Happy Eyeballs may start another TCP attempt while the winning socket is
	// already handshaking TLS. Late callbacks must not regress that stage.
	// A reused HTTP connection may instead fail and cause a fresh dial retry.
	if s.stage == "tls" || s.stage == "http" && !s.reused {
		return
	}
	s.stage = stage
	s.reused = false
}

func (s *traceState) handshaking() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stage == "http" && !s.reused {
		return
	}
	s.stage = "tls"
	s.reused = false
}

func (s *traceState) connected(info httptrace.GotConnInfo) {
	s.mu.Lock()
	s.stage = "http"
	s.reused = info.Reused
	s.mu.Unlock()
}

func (s *traceState) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { s.connecting("dns") },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			if info.Err == nil {
				s.connecting("tcp")
			}
		},
		ConnectStart:      func(string, string) { s.connecting("tcp") },
		TLSHandshakeStart: s.handshaking,
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				s.set("http")
			}
		},
		GotConn:              s.connected,
		WroteRequest:         func(httptrace.WroteRequestInfo) { s.set("http") },
		GotFirstResponseByte: func() { s.set("http") },
	}
}
