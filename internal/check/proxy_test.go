package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"light-prober/internal/protocol"
)

func TestCheckProxyForwardingAndSafeResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer proxy-secret" {
			t.Error("proxy auth missing")
		}
		var monitor protocol.Monitor
		if json.NewDecoder(r.Body).Decode(&monitor) != nil {
			t.Error("invalid forwarding JSON")
		}
		if monitor.CheckProxy != "" || monitor.ICMPProxyURL != "" || monitor.CheckProxyHeaders != nil || monitor.CheckProxyFallback {
			t.Error("recursive delegation/auth fields forwarded")
		}
		if monitor.Headers["Authorization"] != "Bearer target-secret" || monitor.Timeout <= 0 || monitor.Timeout >= 5000 {
			t.Error("target headers or deadline incorrect")
		}
		days := 3.25
		_ = json.NewEncoder(w).Encode(protocol.ProxyResponse{Location: "remote", Status: protocol.ProxyStatus{Up: false, Ping: 1, Err: "SECRET-RESPONSE", Stage: "tls", Code: "expiring", CertificateExpiresAt: 1800000000, CertificateDaysRemaining: &days}})
	}))
	defer server.Close()
	checker := New()
	defer checker.Close()
	result := checker.Check(context.Background(), protocol.Monitor{Method: "SSL_CERT", Target: "https://example.invalid", CheckProxy: server.URL, ICMPProxyURL: server.URL, CheckProxyFallback: true, Headers: map[string]string{"Authorization": "Bearer target-secret"}, CheckProxyHeaders: map[string]string{"Authorization": "Bearer proxy-secret"}})
	assertFailure(t, result, "tls", "expiring")
	if result.CertificateDaysRemaining == nil || *result.CertificateDaysRemaining != 3.25 || result.CertificateExpiresAt != 1800000000 || strings.Contains(result.Message, "SECRET") {
		t.Fatalf("proxy metadata %+v", result)
	}
}

func TestProxyProtocolBoundsAndFallback(t *testing.T) {
	for _, body := range []string{`{}`, `{"location":"x","status":{"up":true,"ping":0}}`, `{"location":"x","status":{"up":true,"ping":-1,"err":""}}`, strings.Repeat("x", 65537)} {
		t.Run(body[:min(len(body), 30)], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			checker := New()
			defer checker.Close()
			assertFailure(t, checker.Check(context.Background(), protocol.Monitor{CheckProxy: server.URL, Target: "http://target"}), "proxy", "invalid_response")
		})
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer proxy.Close()
	checker := New()
	defer checker.Close()
	if result := checker.Check(context.Background(), protocol.Monitor{Target: target.URL, CheckProxy: proxy.URL, CheckProxyFallback: true}); !result.Up {
		t.Fatalf("direct fallback failed %+v", result)
	}
	assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Target: target.URL, CheckProxy: proxy.URL}), "proxy", "status")
}

func TestLegacyFlatICMPProxy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"up":true,"latency_ms":1.25,"icmp_latency_ms":0.5}`)
	}))
	defer server.Close()
	checker := New()
	defer checker.Close()
	result := checker.Check(context.Background(), protocol.Monitor{Method: "ICMP_PING", Target: "localhost", ICMPProxyURL: server.URL})
	if !result.Up || result.ICMPLatencyMS == nil || *result.ICMPLatencyMS != 0.5 {
		t.Fatalf("flat ICMP proxy response rejected %+v", result)
	}
	assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Method: "GET", Target: "http://localhost", CheckProxy: server.URL}), "proxy", "invalid_response")
}

func TestDedicatedICMPProxyLegacyHeadersAndCanonicalIsolation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		canonical map[string]string
		generic   bool
		want      string
	}{
		{"legacy", nil, false, "Bearer legacy"},
		{"empty-canonical", map[string]string{}, false, "Bearer legacy"},
		{"canonical", map[string]string{"Authorization": "Bearer canonical"}, false, "Bearer canonical"},
		{"generic-no-legacy", nil, true, ""},
		{"generic-canonical", map[string]string{"Authorization": "Bearer canonical"}, true, "Bearer canonical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != tc.want {
					t.Fatalf("proxy Authorization=%q want%q", r.Header.Get("Authorization"), tc.want)
				}
				var forwarded protocol.Monitor
				if json.NewDecoder(r.Body).Decode(&forwarded) != nil {
					t.Error("invalid forwarded JSON")
				}
				if len(forwarded.Headers) != 0 || len(forwarded.CheckProxyHeaders) != 0 {
					t.Error("ICMP proxy auth was forwarded as target JSON headers")
				}
				_ = json.NewEncoder(w).Encode(protocol.ProxyResponse{Location: "icmp", Status: protocol.ProxyStatus{Up: true, Ping: 1, Err: ""}})
			}))
			defer server.Close()
			monitor := protocol.Monitor{Method: "ICMP_PING", Target: "localhost", ICMPProxyURL: server.URL, Headers: map[string]string{"Authorization": "Bearer legacy"}, CheckProxyHeaders: tc.canonical}
			if tc.generic {
				monitor.CheckProxy = server.URL
			}
			checker := New()
			defer checker.Close()
			if result := checker.Check(context.Background(), monitor); !result.Up {
				t.Fatalf("ICMP proxy failed %+v", result)
			}
		})
	}
}

func TestLegacyFlatICMPProxyLatencyRetainedAsRTT(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"up":true,"latency_ms":1.25}`)
	}))
	defer server.Close()
	checker := New()
	defer checker.Close()
	result := checker.Check(context.Background(), protocol.Monitor{Method: "ICMP_PING", Target: "localhost", ICMPProxyURL: server.URL})
	if !result.Up || result.ICMPLatencyMS == nil || *result.ICMPLatencyMS != 1.25 {
		t.Fatalf("legacy RTT dropped %+v", result)
	}
}

func TestHTTPSProxyCertificateFailureAndTrustedConnection(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.ProxyResponse{Location: "https", Status: protocol.ProxyStatus{Up: true, Ping: 1, Err: ""}})
	}))
	defer server.Close()
	checker := New()
	defer checker.Close()
	monitor := protocol.Monitor{Method: "SSL_CERT", Target: "https://target.example", CheckProxy: server.URL}
	assertFailure(t, checker.Check(context.Background(), monitor), "proxy", "certificate")
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	checker.transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if result := checker.Check(context.Background(), monitor); !result.Up {
		t.Fatalf("trusted HTTPS proxy failed %+v", result)
	}
}
