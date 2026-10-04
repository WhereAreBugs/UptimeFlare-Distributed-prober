package checkproxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"light-prober/internal/check"
	"light-prober/internal/protocol"
)

const testToken = "proxy-test-token-at-least-16"

func proxyServer(t *testing.T, concurrency int) *httptest.Server {
	t.Helper()
	h, err := New(Options{Token: testToken, Location: "test-site", Concurrency: concurrency, MaxTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	return server
}

func proxyRequest(t *testing.T, server *httptest.Server, path, method, body, token string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestProxyHTTPAndTCPDelegationNoRecursion(t *testing.T) {
	server := proxyServer(t, 2)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer target-secret" {
			t.Error("target auth incorrect")
		}
		_, _ = io.WriteString(w, "healthy")
	}))
	defer target.Close()
	checker := check.New()
	defer checker.Close()
	result := checker.Check(context.Background(), protocol.Monitor{ID: "http", Target: target.URL, CheckProxy: server.URL + "/v1/check", CheckProxyHeaders: map[string]string{"Authorization": "Bearer " + testToken}, Headers: map[string]string{"Authorization": "Bearer target-secret"}, ResponseKeyword: "healthy", Timeout: 1000})
	if !result.Up {
		t.Fatalf("real HTTP delegation failed %+v", result)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	body, err := json.Marshal(protocol.Monitor{Method: "TCP_PING", Target: listener.Addr().String(), Timeout: 1000, CheckProxy: server.URL + "/v1/check", ICMPProxyURL: server.URL + "/v1/ping", CheckProxyHeaders: map[string]string{"Authorization": "Bearer WRONG"}})
	if err != nil {
		t.Fatal(err)
	}
	response := proxyRequest(t, server, "/v1/check", "POST", string(body), testToken)
	defer response.Body.Close()
	var wire protocol.ProxyResponse
	if json.NewDecoder(response.Body).Decode(&wire) != nil || response.StatusCode != 200 || !wire.Status.Up || wire.Location != "test-site" {
		t.Fatalf("original monitor recursion was not stripped %+v HTTP%d", wire, response.StatusCode)
	}
}

func TestProxyRealICMPAndLegacyEndpoint(t *testing.T) {
	server := proxyServer(t, 2)
	for _, path := range []string{"/v1/check", "/v1/ping"} {
		t.Run(path, func(t *testing.T) {
			body := `{"method":"ICMP_PING","target":"127.0.0.1","timeout":1000,"timeout_ms":1000}`
			response := proxyRequest(t, server, path, "POST", body, testToken)
			defer response.Body.Close()
			var wire protocol.ProxyResponse
			data, err := io.ReadAll(response.Body)
			if err != nil || json.Unmarshal(data, &wire) != nil || response.StatusCode != 200 {
				t.Fatalf("invalid proxy response %+v", wire)
			}
			if wire.Status.Code == "permission" || wire.Status.Code == "unsupported" {
				t.Skip(wire.Status.Err)
			}
			if !wire.Status.Up || wire.Status.ICMPLatencyMS == nil {
				t.Fatalf("ICMP proxy failed %+v", wire)
			}
			if path == "/v1/ping" {
				var flat struct {
					Up        bool     `json:"up"`
					LatencyMS *float64 `json:"latency_ms"`
				}
				if json.Unmarshal(data, &flat) != nil || !flat.Up || flat.LatencyMS == nil {
					t.Fatal("legacy flat ping response fields missing")
				}
			}
		})
	}
}

func TestProxyTLSErrorCarriesCertificateMetadata(t *testing.T) {
	server := proxyServer(t, 1)
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("TLS check sent HTTP") }))
	defer target.Close()
	checker := check.New()
	defer checker.Close()
	result := checker.Check(context.Background(), protocol.Monitor{Method: "SSL_CERT", Target: target.URL, Timeout: 1000, CheckProxy: server.URL + "/v1/check", CheckProxyHeaders: map[string]string{"Authorization": "Bearer " + testToken}})
	if result.Up || result.Stage != "tls" || result.Code != "certificate" || result.CertificateDaysRemaining == nil || result.CertificateExpiresAt == 0 {
		t.Fatalf("TLS delegation metadata %+v", result)
	}
}

func TestProxyAuthLimitsAndTimeout(t *testing.T) {
	server := proxyServer(t, 1)
	for _, tc := range []struct {
		name, path, method, body, token string
		status                          int
	}{
		{"auth", "/v1/check", "POST", `{}`, "", 401},
		{"method", "/v1/check", "GET", `{}`, testToken, 405},
		{"path", "/other", "POST", `{}`, testToken, 404},
		{"json", "/v1/check", "POST", `{`, testToken, 400},
		{"missing", "/v1/check", "POST", `{}`, testToken, 400},
		{"timeout", "/v1/check", "POST", `{"method":"TCP_PING","target":"127.0.0.1:80","timeout":1001}`, testToken, 400},
		{"legacy-timeout", "/v1/ping", "POST", `{"target":"127.0.0.1","timeout_ms":1001}`, testToken, 400},
		{"monitor-timeout", "/v1/ping", "POST", `{"target":"127.0.0.1","timeout":1001}`, testToken, 400},
		{"oversized", "/v1/check", "POST", strings.Repeat("x", maxRequestBytes+1), testToken, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := proxyRequest(t, server, tc.path, tc.method, tc.body, tc.token)
			defer response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("got HTTP%d want%d", response.StatusCode, tc.status)
			}
		})
	}
}

func TestProxyBoundedConcurrencyAndTargetTimeout(t *testing.T) {
	server := proxyServer(t, 1)
	started := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer target.Close()
	checker := check.New()
	defer checker.Close()
	finished := make(chan protocol.Result, 1)
	monitor := protocol.Monitor{Target: target.URL, Timeout: 200, CheckProxy: server.URL + "/v1/check", CheckProxyHeaders: map[string]string{"Authorization": "Bearer " + testToken}}
	go func() { finished <- checker.Check(context.Background(), monitor) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("target not started")
	}
	response := proxyRequest(t, server, "/v1/check", "POST", `{"method":"TCP_PING","target":"127.0.0.1:80","timeout":1000}`, testToken)
	defer response.Body.Close()
	if response.StatusCode != 503 || response.Header.Get("Retry-After") != "1" {
		t.Fatal("concurrency bound failed")
	}
	select {
	case result := <-finished:
		if result.Stage != "http" || result.Code != "timeout" {
			t.Fatalf("target timeout classified as proxy failure %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("check failed to respect timeout")
	}
}
