package client

import (
	"light-prober/internal/protocol"
	"testing"
)

func TestCertificateThresholdAndGoProxyConfigValidation(t *testing.T) {
	for _, value := range []int{-1, 0, 14, 365, 366} {
		threshold := value
		cfg := protocol.Config{Version: 1, ProbeID: "probe", Monitors: []protocol.Monitor{{ID: "cert", Target: "https://example.com", Method: "SSL_CERT", CertificateExpiryDays: &threshold}}}
		err := ValidateConfig(cfg)
		if (err != nil) != (value < 0 || value > 365) {
			t.Fatalf("threshold%d=%v", value, err)
		}
	}
	for _, endpoint := range []string{"https://proxy.example/v1/check", "http://127.0.0.1/v1/check", "worker://proxy", "globalping://Paris", "https://user:secret@proxy", "https://proxy/#fragment"} {
		cfg := protocol.Config{Version: 1, ProbeID: "probe", Monitors: []protocol.Monitor{{ID: "icmp", Target: "localhost", Method: "ICMP_PING", CheckProxy: endpoint}}}
		err := ValidateConfig(cfg)
		valid := endpoint == "https://proxy.example/v1/check" || endpoint == "http://127.0.0.1/v1/check"
		if (err == nil) != valid {
			t.Fatalf("endpoint%s=%v", endpoint, err)
		}
	}
}
