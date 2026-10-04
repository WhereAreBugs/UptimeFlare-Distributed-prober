package check

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"light-prober/internal/protocol"
)

type proxyWireStatus struct {
	Up                       *bool    `json:"up"`
	Ping                     *float64 `json:"ping"`
	Err                      *string  `json:"err"`
	Stage                    string   `json:"stage"`
	Code                     string   `json:"code"`
	CertificateExpiresAt     int64    `json:"certificate_expires_at"`
	CertificateDaysRemaining *float64 `json:"certificate_days_remaining"`
	ICMPLatencyMS            *float64 `json:"icmp_latency_ms"`
}

func (c *Checker) checkProxy(ctx context.Context, monitor protocol.Monitor, endpoint string, result protocol.Result) protocol.Result {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
		return failed(result, "proxy", "configuration", "Check proxy requires an HTTP or HTTPS endpoint without URL credentials")
	}
	forward := monitor
	forward.CheckProxy, forward.ICMPProxyURL = "", ""
	forward.CheckProxyFallback, forward.CheckProxyHeaders = false, nil
	proxyHeaders := monitor.CheckProxyHeaders
	if strings.EqualFold(monitor.Method, "ICMP_PING") {
		// ICMP has no HTTP target headers. Older dedicated ICMP proxy settings
		// used headers for authentication; do not forward those secrets as JSON.
		forward.Headers = nil
		if monitor.CheckProxy == "" && endpoint == monitor.ICMPProxyURL && len(proxyHeaders) == 0 {
			proxyHeaders = monitor.Headers
		}
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		remaining -= min(250*time.Millisecond, remaining/10)
		forward.Timeout = max(1, int(remaining/time.Millisecond))
	}
	data, err := json.Marshal(forward)
	if err != nil {
		return failed(result, "proxy", "configuration", "Check proxy request is invalid")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return failed(result, "proxy", "configuration", "Check proxy request is invalid")
	}
	for key, value := range proxyHeaders {
		if !validHeaderName(key) || strings.ContainsAny(value, "\r\n\x00") {
			return failed(result, "proxy", "configuration", "Check proxy headers are invalid")
		}
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	state := &traceState{stage: "tcp"}
	req = req.WithContext(context.WithValue(req.Context(), checkContextKey{}, ctx))
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), state.trace()))
	response, err := c.client.Do(req)
	if err != nil {
		failure := failureFor(result, state.current(), err)
		failure.Message, failure.Stage = "Check proxy failed during "+failure.Stage, "proxy"
		return failure
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return failed(result, "proxy", "status", "Check proxy returned an unsuccessful HTTP response")
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil {
		if ctx.Err() != nil {
			return failureFor(result, "proxy", ctx.Err())
		}
		return failed(result, "proxy", "invalid_response", "Check proxy response could not be read")
	}
	if len(data) > 64<<10 {
		return failed(result, "proxy", "invalid_response", "Check proxy response exceeds the size limit")
	}
	var envelope struct {
		Location      *string          `json:"location"`
		Status        *proxyWireStatus `json:"status"`
		Up            *bool            `json:"up"`
		LatencyMS     *float64         `json:"latency_ms"`
		Stage         string           `json:"stage"`
		Code          string           `json:"code"`
		ICMPLatencyMS *float64         `json:"icmp_latency_ms"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return failed(result, "proxy", "invalid_response", "Check proxy returned an invalid response")
	}
	if envelope.Status == nil && strings.EqualFold(monitor.Method, "ICMP_PING") && monitor.ICMPProxyURL == endpoint && envelope.Up != nil && envelope.LatencyMS != nil {
		empty := ""
		envelope.Location = &empty
		if envelope.ICMPLatencyMS == nil {
			envelope.ICMPLatencyMS = envelope.LatencyMS
		}
		envelope.Status = &proxyWireStatus{Up: envelope.Up, Ping: envelope.LatencyMS, Err: &empty, Stage: envelope.Stage, Code: envelope.Code, ICMPLatencyMS: envelope.ICMPLatencyMS}
	}
	if envelope.Location == nil || len(*envelope.Location) > 128 || envelope.Status == nil {
		return failed(result, "proxy", "invalid_response", "Check proxy returned an invalid response")
	}
	status := envelope.Status
	if status.Up == nil || status.Ping == nil || status.Err == nil || !finiteRange(*status.Ping, 0, 120000) ||
		status.CertificateExpiresAt < 0 || status.CertificateExpiresAt > 253402300799 ||
		status.CertificateDaysRemaining != nil && !finiteRange(*status.CertificateDaysRemaining, -4000000, 4000000) ||
		status.ICMPLatencyMS != nil && !finiteRange(*status.ICMPLatencyMS, 0, 120000) {
		return failed(result, "proxy", "invalid_response", "Check proxy returned invalid sample values")
	}
	result.Up = *status.Up
	result.CertificateExpiresAt, result.CertificateDaysRemaining = status.CertificateExpiresAt, status.CertificateDaysRemaining
	result.ICMPLatencyMS = status.ICMPLatencyMS
	if !result.Up {
		result.Stage = sanitizedProxyStage(status.Stage)
		result.Code = sanitizedProxyCode(status.Code)
		// Proxy error text is untrusted and may include target credentials/body.
		result.Message = "Remote check failed during " + result.Stage + " (" + result.Code + ")"
	}
	return result
}

func finiteRange(value, minimum, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= minimum && value <= maximum
}

func sanitizedProxyStage(stage string) string {
	switch stage {
	case "dns", "tcp", "tls", "http", "body", "icmp", "proxy", "configuration":
		return stage
	}
	return "unknown"
}

func sanitizedProxyCode(code string) string {
	switch code {
	case "timeout", "canceled", "refused", "reset", "unreachable", "closed", "not_found", "certificate", "expiring", "permission", "unsupported", "status", "keyword", "too_large", "target", "method", "header", "request", "certificate_threshold":
		return code
	}
	return "unknown"
}
