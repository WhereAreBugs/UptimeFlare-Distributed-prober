package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"light-prober/internal/protocol"
)

func assertFailure(t *testing.T, result protocol.Result, stage, code string) {
	t.Helper()
	if result.Up || result.Stage != stage || result.Code != code {
		t.Fatalf("result = %+v, want down/%s/%s", result, stage, code)
	}
	// Windows clock resolution can produce a real zero-duration config rejection.
	if result.LatencyMS < 0 || result.Time == 0 {
		t.Fatalf("missing timing: %+v", result)
	}
}

func TestTCPChecksAndFailureClassification(t *testing.T) {
	checker := New()
	defer checker.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	result := checker.Check(context.Background(), protocol.Monitor{ID: "tcp", Method: "TCP_PING", Target: address})
	if !result.Up || result.MonitorID != "tcp" || result.Stage != "" {
		t.Fatalf("TCP success = %+v", result)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Method: "TCP_PING", Target: address}), "tcp", "refused")
	assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Method: "TCP_PING", Target: "127.0.0.1"}), "configuration", "target")
	checker.dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.DNSError{Name: "SECRET", Err: "SECRET", IsNotFound: true}
	}
	result = checker.Check(context.Background(), protocol.Monitor{Method: "TCP_PING", Target: address})
	assertFailure(t, result, "dns", "not_found")
	if strings.Contains(result.Message, "SECRET") {
		t.Fatalf("diagnostic leaked DNS details: %s", result.Message)
	}
}

func TestCheckTimeoutDeadline(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout int
		want    time.Duration
	}{
		{"default", 0, 5 * time.Second},
		{"configured", 7500, 7500 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			checker := New()
			defer checker.Close()
			var deadline time.Time
			checker.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				var ok bool
				deadline, ok = ctx.Deadline()
				if !ok {
					t.Error("check context has no deadline")
				}
				return nil, context.Canceled
			}
			start := time.Now()
			checker.Check(context.Background(), protocol.Monitor{Method: "TCP_PING", Target: "127.0.0.1:80", Timeout: test.timeout})
			elapsed := deadline.Sub(start)
			if elapsed < test.want || elapsed > test.want+250*time.Millisecond {
				t.Fatalf("deadline=%s after start, want %s", elapsed, test.want)
			}
		})
	}
}

func TestHTTPStatusKeywordsAndMethods(t *testing.T) {
	var sawPost bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			body, _ := io.ReadAll(r.Body)
			sawPost = string(body) == "post-body" && r.Header.Get("X-Test") == "header-value"
		}
		if r.URL.Path == "/status" {
			w.WriteHeader(503)
		}
		_, _ = w.Write([]byte("healthy endpoint"))
	}))
	defer server.Close()
	checker := New()
	defer checker.Close()
	tests := []struct {
		name    string
		monitor protocol.Monitor
		stage   string
		code    string
	}{
		{"get", protocol.Monitor{Target: server.URL}, "", ""},
		{"post", protocol.Monitor{Target: server.URL, Method: "POST", Headers: map[string]string{"X-Test": "header-value"}, Body: "post-body"}, "", ""},
		{"status", protocol.Monitor{Target: server.URL + "/status"}, "http", "status"},
		{"accepted-status", protocol.Monitor{Target: server.URL + "/status", ExpectedCodes: []int{503}}, "", ""},
		{"keyword-found", protocol.Monitor{Target: server.URL, ResponseKeyword: "healthy"}, "", ""},
		{"keyword-missing", protocol.Monitor{Target: server.URL, ResponseKeyword: "absent"}, "body", "keyword"},
		{"forbidden-found", protocol.Monitor{Target: server.URL, ResponseForbiddenKeyword: "healthy"}, "body", "keyword"},
		{"forbidden-absent", protocol.Monitor{Target: server.URL, ResponseForbiddenKeyword: "absent"}, "", ""},
		{"invalid-header", protocol.Monitor{Target: server.URL, Headers: map[string]string{"X-Test": "secret\r\nInjection: value"}}, "configuration", "header"},
		{"invalid-method", protocol.Monitor{Target: server.URL, Method: "INVALID METHOD"}, "configuration", "method"},
		{"invalid-url", protocol.Monitor{Target: "ftp://user:SECRET@example.invalid/path?token=SECRET"}, "configuration", "target"},
		{"credentials", protocol.Monitor{Target: "https://user:SECRET@example.invalid/path?token=SECRET"}, "configuration", "target"},
		{"invalid-timeout", protocol.Monitor{Target: server.URL, Timeout: -1}, "configuration", "timeout"},
		{"oversized-keyword", protocol.Monitor{Target: server.URL, ResponseKeyword: strings.Repeat("x", maxKeywordSize+1)}, "configuration", "keyword"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := checker.Check(context.Background(), test.monitor)
			if test.stage == "" {
				if !result.Up {
					t.Fatalf("unexpected failure: %+v", result)
				}
			} else {
				assertFailure(t, result, test.stage, test.code)
			}
			if strings.Contains(result.Message, "SECRET") {
				t.Fatal("credential leaked in diagnostic")
			}
		})
	}
	if !sawPost {
		t.Fatal("POST body and headers not sent")
	}
}

func TestRedirectIsReportedWithoutFollowing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/destination", http.StatusFound)
			return
		}
		t.Error("redirect destination was unexpectedly fetched")
	}))
	defer server.Close()
	checker := New()
	defer checker.Close()
	assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Target: server.URL}), "http", "status")
}

func TestTLSVerificationAndTrustedTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secure"))
	}))
	defer server.Close()
	checker := New()
	defer checker.Close()
	assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Target: server.URL}), "tls", "certificate")
	checker.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	checker.transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	result := checker.Check(context.Background(), protocol.Monitor{Target: server.URL})
	if !result.Up {
		t.Fatalf("trusted TLS failed: %+v", result)
	}
}

func TestTimeoutStages(t *testing.T) {
	t.Run("dns", func(t *testing.T) {
		checker := New()
		defer checker.Close()
		checker.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			trace := httptrace.ContextClientTrace(ctx)
			trace.DNSStart(httptrace.DNSStartInfo{Host: "secret.invalid"})
			<-ctx.Done()
			return nil, ctx.Err()
		}
		assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Target: "http://secret.invalid/", Timeout: 30}), "dns", "timeout")
	})
	t.Run("tcp", func(t *testing.T) {
		checker := New()
		defer checker.Close()
		checker.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			trace := httptrace.ContextClientTrace(ctx)
			trace.ConnectStart(network, address)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Target: "http://127.0.0.1:10001/", Timeout: 30}), "tcp", "timeout")
	})
	t.Run("tls", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				accepted <- conn
			}
		}()
		checker := New()
		defer checker.Close()
		assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Target: "https://" + listener.Addr().String(), Timeout: 40}), "tls", "timeout")
		select {
		case conn := <-accepted:
			_ = conn.Close()
		case <-time.After(time.Second):
			t.Fatal("TLS connection never accepted")
		}
	})
	t.Run("http", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		checker := New()
		defer checker.Close()
		assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Target: server.URL, Timeout: 30}), "http", "timeout")
	})
	t.Run("body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer server.Close()
		checker := New()
		defer checker.Close()
		assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Target: server.URL, Timeout: 30, ResponseKeyword: "healthy"}), "body", "timeout")
	})
}

func TestBoundedBodyAndEarlyClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, strings.NewReader(strings.Repeat("x", 2*maxKeywordBody)))
	}))
	defer server.Close()
	checker := New()
	defer checker.Close()
	assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Target: server.URL, ResponseKeyword: "x"}), "body", "too_large")
	if result := checker.Check(context.Background(), protocol.Monitor{Target: server.URL}); !result.Up {
		t.Fatalf("bounded default body drain failed: %+v", result)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer broken.Close()
	assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Target: broken.URL, ResponseKeyword: "short"}), "body", "closed")
}

func TestStreamingKeywordsAcrossChunksAndRepeatedPrefix(t *testing.T) {
	for _, test := range []struct {
		body, keyword, forbidden string
		wanted, blocked          bool
	}{
		{strings.Repeat("x", (32<<10)-3) + "healthy endpoint", "healthy", "forbidden", true, false},
		{"ababababac", "ababac", "abac", true, true},
		{"no matching keyword", "missing", "", false, false},
		{"你好，健康的端点", "健康", "错误", true, false},
	} {
		wanted, blocked, exhausted, err := scanKeywords(strings.NewReader(test.body), test.keyword, test.forbidden)
		if err != nil || exhausted || wanted != test.wanted || blocked != test.blocked {
			t.Fatalf("scan = %v/%v/%v/%v", wanted, blocked, exhausted, err)
		}
	}
}

func TestDetachedHTTPDialRetainsProbeDeadlineAndCancellation(t *testing.T) {
	original, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	detached := context.WithoutCancel(context.WithValue(original, checkContextKey{}, original))
	started, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := boundedHTTPDial(func(ctx context.Context, _, _ string) (net.Conn, error) {
			deadline, ok := ctx.Deadline()
			expected, _ := original.Deadline()
			if !ok || !deadline.Equal(expected) {
				t.Errorf("HTTP background dial lost original deadline: %v, %v", deadline, ok)
			}
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}, detached, "tcp", "unused")
		finished <- err
	}()
	<-started
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dial cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled probe left a background dial running")
	}
}

func TestSanitizedNetworkErrors(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{syscall.ECONNREFUSED, "refused"},
		{syscall.ECONNRESET, "reset"},
		{syscall.ENETUNREACH, "unreachable"},
		{context.Canceled, "canceled"},
		{errors.New("secret URL, secret headers, secret body"), "unknown"},
	} {
		result := failureFor(protocol.Result{}, "tcp", test.err)
		if result.Code != test.code || strings.Contains(result.Message, "secret") {
			t.Fatalf("classification = %+v", result)
		}
	}
}

func TestConcurrentCheckerAndTrace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	checker := New()
	defer checker.Close()
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			result := checker.Check(context.Background(), protocol.Monitor{Target: server.URL, Timeout: 1000})
			if !result.Up {
				t.Errorf("concurrent check failed: %+v", result)
			}
		}()
	}
	group.Wait()
	state := &traceState{stage: "tcp"}
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			state.set("http")
			_ = state.current()
		}()
	}
	group.Wait()
}

func TestLateDialCallbacksDoNotRegressTLSAndHTTP(t *testing.T) {
	state := &traceState{stage: "tcp"}
	state.handshaking()
	state.connecting("tcp")
	state.connecting("dns")
	if got := state.current(); got != "tls" {
		t.Fatalf("late connection callback regressed TLS to %s", got)
	}
	state.connected(httptrace.GotConnInfo{})
	state.connecting("tcp")
	state.handshaking()
	if got := state.current(); got != "http" {
		t.Fatalf("late connection callback regressed HTTP to %s", got)
	}
	state.connected(httptrace.GotConnInfo{Reused: true})
	state.connecting("tcp")
	if got := state.current(); got != "tcp" {
		t.Fatalf("retry on a reused connection did not record fresh TCP dial: %s", got)
	}
}

func BenchmarkHTTPCheck(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	checker := New()
	defer checker.Close()
	monitor := protocol.Monitor{Target: server.URL}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if result := checker.Check(context.Background(), monitor); !result.Up {
			b.Fatal(result)
		}
	}
}

func BenchmarkTCPCheck(b *testing.B) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	checker := New()
	defer checker.Close()
	monitor := protocol.Monitor{Method: "TCP_PING", Target: listener.Addr().String()}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if result := checker.Check(context.Background(), monitor); !result.Up {
			b.Fatal(result)
		}
	}
}
