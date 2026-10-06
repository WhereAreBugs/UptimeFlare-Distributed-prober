//go:build !nootel

package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracecollect "go.opentelemetry.io/proto/otlp/collector/trace/v1"
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
	r.Check(ctx, protocol.Result{MonitorID: "SECRET-MONITOR", Up: false, LatencyMS: 123, Stage: "tcp", Code: "refused", Message: "SECRET-MESSAGE"}, "TCP_PING")
	days, rtt := 3.0, 0.5
	r.Check(ctx, protocol.Result{Up: false, Stage: "tls", CertificateDaysRemaining: &days}, "SSL_CERT")
	r.Check(ctx, protocol.Result{Up: true, ICMPLatencyMS: &rtt}, "ICMP_PING")
	r.Check(ctx, protocol.Result{Stage: "SECRET-STAGE"}, "SECRET-METHOD")
	r.Operation(ctx, "check.execute", 5*time.Second, nil)
	r.Queue(3, 300)
	r.Upload(ctx, time.Millisecond, 100, true)
	if err = r.(*recorder).provider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	request := <-observed
	text := request.String()
	for _, name := range []string{"probe.checks", "probe.check.duration", "probe.runtime.allocations", "probe.runtime.gc.cycles", "probe.runtime.memory", "SSL_CERT", "ICMP_PING", "probe.upload.bytes", "probe.queue.results", "probe.runtime.heap", "probe.runtime.goroutines"} {
		if !strings.Contains(text, name) {
			t.Fatalf("metric %s not exported", name)
		}
	}
	if strings.Contains(text, "SECRET-") || strings.Contains(text, `key:"up"`) || strings.Contains(text, `key:"stage"`) || strings.Contains(text, "certificate.remaining") {
		t.Fatal("high-cardinality target data leaked into telemetry")
	}
	if !strings.Contains(text, "probe.id") || !strings.Contains(text, "sg") {
		t.Fatal("probe identity missing from metrics")
	}
	if err = r.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTraceExportPropagationAndSampling(t *testing.T) {
	requests := make(chan *tracecollect.ExportTraceServiceRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		defer reader.Close()
		data, _ := io.ReadAll(reader)
		if r.URL.Path == "/v1/traces" {
			var v tracecollect.ExportTraceServiceRequest
			if err := proto.Unmarshal(data, &v); err != nil {
				t.Error(err)
			}
			requests <- &v
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_TRACES_SAMPLER_ARG", "1")
	r, err := New(context.Background(), true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r.Identity("probe-test")
	ctx, end := r.Start(context.Background(), "upload.batch")
	child, finish := r.Start(ctx, "receiver.POST")
	h := http.Header{}
	r.Inject(child, h)
	propagated := propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(h))
	remote := trace.SpanContextFromContext(propagated)
	if !remote.IsRemote() || !remote.IsSampled() {
		t.Fatal("W3C trace context missing")
	}
	finish(errors.New("SECRET-ERROR"))
	end(nil)
	if err := r.(*recorder).traces.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := <-requests
	spans := got.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("wanted parent and child, got %d", len(spans))
	}
	if !bytes.Equal(spans[0].ParentSpanId, spans[1].SpanId) || !bytes.Equal(spans[0].TraceId, spans[1].TraceId) || hex.EncodeToString(spans[0].SpanId) != remote.SpanID().String() {
		t.Fatal("trace parent/child relationship lost")
	}
	if strings.Contains(got.String(), "SECRET") {
		t.Fatal("error detail leaked")
	}
	_ = r.Shutdown(context.Background())
	t.Setenv("OTEL_TRACES_SAMPLER", "always_off")
	off, err := New(context.Background(), true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, finish = off.Start(propagated, "disabled")
	finish(nil)
	_ = off.Shutdown(context.Background())
	select {
	case <-requests:
		t.Fatal("disabled tracing exported spans")
	default:
	}
}
