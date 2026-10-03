//go:build !nootel

package telemetry

import (
	"context"
	"errors"
	"runtime/metrics"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"light-prober/internal/protocol"
)

type recorder struct {
	identity       atomic.Value
	provider       *sdk.MeterProvider
	checks         metric.Int64Counter
	checkDuration  metric.Float64Histogram
	uploads        metric.Int64Counter
	uploadDuration metric.Float64Histogram
	wireBytes      metric.Int64Counter
	configFailures metric.Int64Counter
	queued         atomic.Int64
	queueBytes     atomic.Int64
}

// New uses standard OTEL_EXPORTER_OTLP_* variables. Disabled means no SDK, reader, or network work.
func New(ctx context.Context, enabled bool, interval time.Duration) (Recorder, error) {
	if !enabled {
		return noop{}, nil
	}
	if interval < 10*time.Second {
		return nil, errors.New("telemetry interval must be at least 10s")
	}
	exporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression), otlpmetrichttp.WithTimeout(5*time.Second))
	if err != nil {
		return nil, errors.New("cannot configure OTLP HTTP metric exporter")
	}
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", "light-prober")), resource.WithFromEnv())
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, errors.New("invalid OpenTelemetry resource configuration")
	}
	r := &recorder{provider: sdk.NewMeterProvider(
		sdk.WithResource(res),
		sdk.WithCardinalityLimit(64), sdk.WithReader(sdk.NewPeriodicReader(exporter, sdk.WithInterval(interval), sdk.WithTimeout(5*time.Second))),
		sdk.WithView(sdk.NewView(sdk.Instrument{Kind: sdk.InstrumentKindHistogram}, sdk.Stream{Aggregation: sdk.AggregationExplicitBucketHistogram{Boundaries: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000, 10000, 30000, 120000}}})),
	)}
	r.identity.Store("unassigned")
	meter := r.provider.Meter("light-prober")
	r.checks, _ = meter.Int64Counter("probe.checks", metric.WithUnit("{check}"))
	r.checkDuration, _ = meter.Float64Histogram("probe.check.duration", metric.WithUnit("ms"))
	r.uploads, _ = meter.Int64Counter("probe.uploads", metric.WithUnit("{batch}"))
	r.uploadDuration, _ = meter.Float64Histogram("probe.upload.duration", metric.WithUnit("ms"))
	r.wireBytes, _ = meter.Int64Counter("probe.upload.bytes", metric.WithUnit("By"))
	r.configFailures, _ = meter.Int64Counter("probe.config.failures")
	queued, _ := meter.Int64ObservableGauge("probe.queue.results")
	queueBytes, _ := meter.Int64ObservableGauge("probe.queue.bytes", metric.WithUnit("By"))
	heap, _ := meter.Int64ObservableGauge("probe.runtime.heap", metric.WithUnit("By"))
	goroutines, _ := meter.Int64ObservableGauge("probe.runtime.goroutines")
	gcCPU, _ := meter.Float64ObservableCounter("probe.runtime.gc.cpu", metric.WithUnit("s"))
	samples := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/sched/goroutines:goroutines"}, {Name: "/cpu/classes/gc/total:cpu-seconds"}}
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		metrics.Read(samples)
		attrs := metric.WithAttributes(attribute.String("probe.id", r.identity.Load().(string)))
		o.ObserveInt64(queued, r.queued.Load(), attrs)
		o.ObserveInt64(queueBytes, r.queueBytes.Load(), attrs)
		o.ObserveInt64(heap, int64(samples[0].Value.Uint64()), attrs)
		o.ObserveInt64(goroutines, int64(samples[1].Value.Uint64()), attrs)
		o.ObserveFloat64(gcCPU, samples[2].Value.Float64(), attrs)
		return nil
	}, queued, queueBytes, heap, goroutines, gcCPU)
	if err != nil {
		_ = r.provider.Shutdown(ctx)
		return nil, err
	}
	return r, nil
}
func (r *recorder) Identity(id string) {
	if id != "" {
		r.identity.Store(id)
	}
}
func (r *recorder) Check(ctx context.Context, v protocol.Result) {
	stage := v.Stage
	if stage == "" {
		stage = "ok"
	}
	attrs := metric.WithAttributes(attribute.Bool("up", v.Up), attribute.String("stage", stage), attribute.String("probe.id", r.identity.Load().(string)))
	r.checks.Add(ctx, 1, attrs)
	r.checkDuration.Record(ctx, v.LatencyMS, attrs)
}
func (r *recorder) Upload(ctx context.Context, d time.Duration, bytes int, ok bool) {
	attrs := metric.WithAttributes(attribute.Bool("success", ok), attribute.String("probe.id", r.identity.Load().(string)))
	r.uploads.Add(ctx, 1, attrs)
	r.uploadDuration.Record(ctx, float64(d)/float64(time.Millisecond), attrs)
	r.wireBytes.Add(ctx, int64(bytes), attrs)
}
func (r *recorder) Queue(n uint64, b int64) { r.queued.Store(int64(n)); r.queueBytes.Store(b) }
func (r *recorder) ConfigFailure(ctx context.Context) {
	r.configFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("probe.id", r.identity.Load().(string))))
}
func (r *recorder) Shutdown(ctx context.Context) error { return r.provider.Shutdown(ctx) }
