// Package protocol defines the versioned contract shared with the UptimeFlare receiver.
package protocol

const (
	Version                      = 1
	MaxBatchResults              = 200
	MaxMonitors                  = 500
	DefaultIntervalSeconds       = 300
	MinIntervalSeconds           = 60
	MaxIntervalSeconds           = 86400
	DefaultTimeoutMS             = 5000
	DefaultCertificateExpiryDays = 14
)

type Monitor struct {
	ID                       string            `json:"id"`
	Method                   string            `json:"method"`
	Target                   string            `json:"target"`
	Timeout                  int               `json:"timeout,omitempty"`         // milliseconds
	IntervalSeconds          int               `json:"intervalSeconds,omitempty"` // seconds; zero uses the legacy fallback
	Headers                  map[string]string `json:"headers,omitempty"`
	Body                     string            `json:"body,omitempty"`
	ExpectedCodes            []int             `json:"expectedCodes,omitempty"`
	ResponseKeyword          string            `json:"responseKeyword,omitempty"`
	ResponseForbiddenKeyword string            `json:"responseForbiddenKeyword,omitempty"`
	CertificateExpiryDays    *int              `json:"certificateExpiryDays,omitempty"`
	ICMPProxyURL             string            `json:"icmpProxyURL,omitempty"`
	CheckProxy               string            `json:"checkProxy,omitempty"`
	CheckProxyFallback       bool              `json:"checkProxyFallback,omitempty"`
	CheckProxyHeaders        map[string]string `json:"checkProxyHeaders,omitempty"`
}

type Config struct {
	Version  int       `json:"version"`
	ProbeID  string    `json:"probe_id"`
	Monitors []Monitor `json:"monitors"`
}

type Result struct {
	MonitorID                string   `json:"monitor_id"`
	Time                     int64    `json:"time"` // Unix seconds, time of check start
	Up                       bool     `json:"up"`
	LatencyMS                float64  `json:"latency_ms"`
	Stage                    string   `json:"stage,omitempty"`
	Code                     string   `json:"code,omitempty"`
	Message                  string   `json:"message,omitempty"`
	CertificateExpiresAt     int64    `json:"certificate_expires_at,omitempty"`
	CertificateDaysRemaining *float64 `json:"certificate_days_remaining,omitempty"`
	ICMPLatencyMS            *float64 `json:"icmp_latency_ms,omitempty"`
}

// ProxyResponse matches the UptimeFlare HTTP check-proxy response contract.
type ProxyResponse struct {
	Location string      `json:"location"`
	Status   ProxyStatus `json:"status"`
}

type ProxyStatus struct {
	Up                       bool     `json:"up"`
	Ping                     float64  `json:"ping"`
	Err                      string   `json:"err"`
	Stage                    string   `json:"stage,omitempty"`
	Code                     string   `json:"code,omitempty"`
	CertificateExpiresAt     int64    `json:"certificate_expires_at,omitempty"`
	CertificateDaysRemaining *float64 `json:"certificate_days_remaining,omitempty"`
	ICMPLatencyMS            *float64 `json:"icmp_latency_ms,omitempty"`
}

type Batch struct {
	Version int      `json:"version"`
	BatchID string   `json:"batch_id"`
	Results []Result `json:"results"`
}

type Ack struct {
	BatchID  string `json:"batch_id"`
	Accepted int    `json:"accepted"`
}
