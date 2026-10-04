// Package checkproxy exposes bounded network checks behind bearer authentication.
package checkproxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"light-prober/internal/check"
	"light-prober/internal/client"
	"light-prober/internal/protocol"
)

const maxRequestBytes = 1 << 20

type Options struct {
	Token       string
	Location    string
	Concurrency int
	MaxTimeout  time.Duration
	RootCAs     *x509.CertPool
}

type Handler struct {
	checker    *check.Checker
	tokenHash  [32]byte
	location   string
	gate       chan struct{}
	maxTimeout time.Duration
}

func New(options Options) (*Handler, error) {
	if len(options.Token) < 16 || strings.TrimSpace(options.Token) != options.Token || strings.ContainsAny(options.Token, "\r\n\x00") {
		return nil, errors.New("proxy token must contain at least 16 characters and no surrounding whitespace")
	}
	if options.Location == "" || len(options.Location) > 128 || strings.ContainsAny(options.Location, "\r\n\x00") {
		return nil, errors.New("proxy location must contain 1..128 characters")
	}
	if options.Concurrency < 1 || options.Concurrency > 32 {
		return nil, errors.New("proxy concurrency must be 1..32")
	}
	if options.MaxTimeout < time.Millisecond || options.MaxTimeout > 120*time.Second {
		return nil, errors.New("proxy max-timeout must be 1ms..120s")
	}
	return &Handler{checker: check.NewWithRootCAs(options.RootCAs), tokenHash: sha256.Sum256([]byte("Bearer " + options.Token)), location: options.Location, gate: make(chan struct{}, options.Concurrency), maxTimeout: options.MaxTimeout}, nil
}

func (h *Handler) Close() { h.checker.Close() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hash := sha256.Sum256([]byte(r.Header.Get("Authorization")))
	if subtle.ConstantTimeCompare(hash[:], h.tokenHash[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/v1/check" && r.URL.Path != "/v1/ping" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	select {
	case h.gate <- struct{}{}:
		defer func() { <-h.gate }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "proxy is busy", http.StatusServiceUnavailable)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid request", http.StatusBadRequest)
		}
		return
	}
	var monitor protocol.Monitor
	if r.URL.Path == "/v1/ping" {
		var legacy struct {
			Target    string `json:"target"`
			TimeoutMS int    `json:"timeout_ms"`
			Timeout   int    `json:"timeout"`
		}
		if json.Unmarshal(data, &legacy) != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if legacy.TimeoutMS != 0 {
			legacy.Timeout = legacy.TimeoutMS
		}
		monitor = protocol.Monitor{ID: "proxy", Method: "ICMP_PING", Target: legacy.Target, Timeout: legacy.Timeout}
	} else if json.Unmarshal(data, &monitor) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if monitor.ID == "" {
		monitor.ID = "proxy"
	}
	// A delegated check always executes here. Worker clients send their original
	// monitor including delegation fields; removing them prevents recursive hops.
	monitor.CheckProxy, monitor.ICMPProxyURL, monitor.CheckProxyFallback, monitor.CheckProxyHeaders = "", "", false, nil
	if client.ValidateConfig(protocol.Config{Version: protocol.Version, ProbeID: "proxy", Monitors: []protocol.Monitor{monitor}}) != nil {
		http.Error(w, "invalid monitor", http.StatusBadRequest)
		return
	}
	if monitor.Timeout == 0 {
		monitor.Timeout = protocol.DefaultTimeoutMS
	}
	if time.Duration(monitor.Timeout)*time.Millisecond > h.maxTimeout {
		http.Error(w, "check timeout exceeds proxy limit", http.StatusBadRequest)
		return
	}
	// Leave a small budget to encode and deliver the target's timeout result.
	monitor.Timeout -= min(100, monitor.Timeout/10)
	result := h.checker.Check(r.Context(), monitor)
	if r.Context().Err() != nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	response := protocol.ProxyResponse{Location: h.location, Status: protocol.ProxyStatus{Up: result.Up, Ping: result.LatencyMS, Err: result.Message, Stage: result.Stage, Code: result.Code, CertificateExpiresAt: result.CertificateExpiresAt, CertificateDaysRemaining: result.CertificateDaysRemaining, ICMPLatencyMS: result.ICMPLatencyMS}}
	if r.URL.Path == "/v1/ping" {
		// Legacy ICMP clients read flat fields, while generic check-proxy clients
		// read status. Carry both without changing /v1/check's established shape.
		_ = json.NewEncoder(w).Encode(struct {
			protocol.ProxyResponse
			Up            bool     `json:"up"`
			LatencyMS     float64  `json:"latency_ms"`
			Stage         string   `json:"stage,omitempty"`
			Code          string   `json:"code,omitempty"`
			Message       string   `json:"message,omitempty"`
			ICMPLatencyMS *float64 `json:"icmp_latency_ms,omitempty"`
		}{response, result.Up, result.LatencyMS, result.Stage, result.Code, result.Message, result.ICMPLatencyMS})
		return
	}
	_ = json.NewEncoder(w).Encode(response)
}
