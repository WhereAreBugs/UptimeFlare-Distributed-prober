//go:build !nootel

package telemetry

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	collect "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
	"light-prober/internal/protocol"
)

func TestOTLPExportAndDisabledPath(t *testing.T) {
	ctx := context.Background()
	disabled, err := New(ctx, false, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := disabled.(noop); !ok {
		t.Fatal("disabled telemetry initialized SDK")
	}
	observed := make(chan *collect.ExportMetricsServiceRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" || r.Header.Get("Content-Encoding") != "gzip" {
			t.Error("wrong OTLP path or compression")
		}
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		defer reader.Close()
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Error(err)
			return
		}
		var request collect.ExportMetricsServiceRequest
		if err = proto.Unmarshal(data, &request); err != nil {
			t.Error(err)
			return
		}
		observed <- &request
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", server.URL+"/v1/metrics")
	r, err := New(ctx, true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r.Identity("sg")
	r.Check(ctx, protocol.Result{MonitorID: "SECRET-MONITOR", Up: false, LatencyMS: 123, Stage: "tcp", Code: "refused", Message: "SECRET-MESSAGE"})
	r.Queue(3, 300)
	r.Upload(ctx, time.Millisecond, 100, true)
	if err = r.(*recorder).provider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	request := <-observed
	text := request.String()
	for _, name := range []string{"probe.checks", "probe.check.duration", "probe.upload.bytes", "probe.queue.results", "probe.runtime.heap", "probe.runtime.goroutines"} {
		if !strings.Contains(text, name) {
			t.Fatalf("metric %s not exported", name)
		}
	}
	if strings.Contains(text, "SECRET-") {
		t.Fatal("high-cardinality target data leaked into telemetry")
	}
	if !strings.Contains(text, "probe.id") || !strings.Contains(text, "sg") {
		t.Fatal("probe identity missing from metrics")
	}
	if err = r.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
