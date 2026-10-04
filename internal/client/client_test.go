package client

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"light-prober/internal/protocol"
)

func TestBatchCompressionAndAcknowledgement(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "identity", true: "gzip"}[compressed], func(t *testing.T) {
			var ids []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					t.Error("missing auth")
				}
				var reader io.Reader = r.Body
				if compressed {
					if r.Header.Get("Content-Encoding") != "gzip" {
						t.Error("missing gzip")
					}
					gz, err := gzip.NewReader(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					defer gz.Close()
					reader = gz
				}
				var batch protocol.Batch
				if err := json.NewDecoder(reader).Decode(&batch); err != nil {
					t.Error(err)
					return
				}
				if len(batch.Results) != 1 || batch.Results[0].MonitorID != "web" {
					t.Error("wrong sample")
				}
				ids = append(ids, batch.BatchID)
				_ = json.NewEncoder(w).Encode(protocol.Ack{BatchID: batch.BatchID, Accepted: 1})
			}))
			defer server.Close()
			c, err := New(server.URL, "test-token", compressed, true)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			results := []protocol.Result{{MonitorID: "web", Time: 1800000000, Up: true, LatencyMS: 1}}
			for i := 0; i < 2; i++ {
				if bytes, err := c.Upload(context.Background(), results); err != nil || bytes == 0 {
					t.Fatalf("upload bytes=%d err=%v", bytes, err)
				}
			}
			if ids[0] != ids[1] || len(ids[0]) != 64 {
				t.Fatal("retry ID not deterministic")
			}
		})
	}
}

func TestRejectMismatchedAckAndRedirect(t *testing.T) {
	for _, status := range []int{200, 302, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://invalid.example")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"batch_id":"wrong","accepted":1}`)
			}))
			defer server.Close()
			c, _ := New(server.URL, "test-token", true, true)
			defer c.Close()
			if _, err := c.Upload(context.Background(), []protocol.Result{{MonitorID: "web", Time: 1800000000, Up: true}}); err == nil {
				t.Fatal("invalid ACK accepted")
			}
		})
	}
}

func TestServerValidation(t *testing.T) {
	for _, server := range []string{"http://example.com", "https://u:p@example.com", "https://example.com/?token=secret", "https://example.com/#fragment", "ftp://example.com", "nonsense"} {
		if _, err := New(server, "test", true, false); err == nil {
			t.Fatalf("accepted %q", server)
		}
	}
	if _, err := New("https://example.com", "x\r\ny", true, false); err == nil {
		t.Fatal("header injection accepted")
	}
}

func TestConfigAndResponseLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat(" ", maxResponse+1))
	}))
	defer server.Close()
	c, _ := New(server.URL, "test", true, true)
	defer c.Close()
	if _, err := c.Config(context.Background()); err == nil {
		t.Fatal("unbounded response accepted")
	}
	if err := ValidateConfig(protocol.Config{Version: 1, ProbeID: "p", Monitors: []protocol.Monitor{{ID: "a", Target: "x"}, {ID: "a", Target: "y"}}}); err == nil {
		t.Fatal("duplicate monitor accepted")
	}
}

func TestValidateConfigInterval(t *testing.T) {
	for _, interval := range []int{-86400, -1, 1, 59, 86401} {
		t.Run("invalid_"+strconv.Itoa(interval), func(t *testing.T) {
			cfg := protocol.Config{Version: protocol.Version, ProbeID: "p", Monitors: []protocol.Monitor{{ID: "web", Target: "https://example.com", IntervalSeconds: interval}}}
			if err := ValidateConfig(cfg); err == nil {
				t.Fatal("invalid monitor interval accepted")
			}
		})
	}
	for _, interval := range []int{0, 60, 300, 86400} {
		t.Run("valid_"+strconv.Itoa(interval), func(t *testing.T) {
			cfg := protocol.Config{Version: protocol.Version, ProbeID: "p", Monitors: []protocol.Monitor{{ID: "web", Target: "https://example.com", IntervalSeconds: interval}}}
			if err := ValidateConfig(cfg); err != nil {
				t.Fatalf("valid monitor interval rejected: %v", err)
			}
		})
	}
}

func TestConfigIntervalWireCompatibility(t *testing.T) {
	for _, test := range []struct {
		name     string
		json     string
		interval int
	}{
		{"omitted", `{"id":"web","target":"https://example.com"}`, 0},
		{"configured", `{"id":"web","target":"https://example.com","intervalSeconds":60}`, 60},
	} {
		t.Run(test.name, func(t *testing.T) {
			var monitor protocol.Monitor
			if err := json.Unmarshal([]byte(test.json), &monitor); err != nil {
				t.Fatal(err)
			}
			if monitor.IntervalSeconds != test.interval {
				t.Fatalf("intervalSeconds=%d, want %d", monitor.IntervalSeconds, test.interval)
			}
			cfg := protocol.Config{Version: protocol.Version, ProbeID: "p", Monitors: []protocol.Monitor{monitor}}
			if err := ValidateConfig(cfg); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(monitor)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"intervalSeconds"`) != (test.interval != 0) {
				t.Fatalf("unexpected intervalSeconds encoding: %s", data)
			}
		})
	}
}

func TestLargeDistributedConfigurationRemainsBounded(t *testing.T) {
	cfg := protocol.Config{Version: protocol.Version, ProbeID: "fixture"}
	for i := 0; i < protocol.MaxMonitors; i++ {
		cfg.Monitors = append(cfg.Monitors, protocol.Monitor{ID: strconv.Itoa(i), Target: "https://example.test", Method: "GET", IntervalSeconds: 300, Timeout: 5000})
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Monitors = append(cfg.Monitors, protocol.Monitor{ID: "excess", Target: "https://example.test"})
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("accepted more than the shared monitor limit")
	}
}
