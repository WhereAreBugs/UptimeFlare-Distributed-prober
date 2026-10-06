//go:build !nootel

package telemetry

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdk "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"light-prober/internal/protocol"
)

type recorder struct {
	identity                                                           atomic.Value
	provider                                                           *sdk.MeterProvider
	traces                                                             *sdktrace.TracerProvider
	tracer                                                             trace.Tracer
	checks                                                             metric.Int64Counter
	checkDuration                                                      metric.Float64Histogram
	uploads                                                            metric.Int64Counter
	uploadDuration                                                     metric.Float64Histogram
	wireBytes                                                          metric.Int64Counter
	configFailures                                                     metric.Int64Counter
	operations                                                         metric.Int64Counter
	operationDuration                                                  metric.Float64Histogram
	queued, queueBytes, databaseBytes, databaseLimit, oldestAt, active atomic.Int64
}

// New exports operational metrics. Traces use explicit standard endpoint configuration
// and parent-based sampling; no SDK or exporter is created on the disabled path.
func New(ctx context.Context, enabled bool, interval time.Duration, version ...string) (Recorder, error) {
	if !enabled {
		return noop{}, nil
	}
	if interval < 10*time.Second {
		return nil, errors.New("telemetry interval must be at least 10s")
	}
	rate := 0.05
	if value := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); value != "" {
		var err error
		rate, err = strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(rate) || rate < 0 || rate > 1 {
			return nil, errors.New("OTEL_TRACES_SAMPLER_ARG must be 0..1")
		}
	}
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(rate))
	switch os.Getenv("OTEL_TRACES_SAMPLER") {
	case "", "parentbased_traceidratio":
	case "always_off":
		sampler = sdktrace.NeverSample()
	case "always_on":
		sampler = sdktrace.AlwaysSample()
	default:
		return nil, errors.New("supported samplers: parentbased_traceidratio, always_off, always_on")
	}
	exporter, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression), otlpmetrichttp.WithTimeout(5*time.Second))
	if err != nil {
		return nil, errors.New("cannot configure OTLP HTTP metric exporter")
	}
	serviceVersion := "dev"
	if len(version) > 0 {
		serviceVersion = version[0]
	}
	res, err := resource.New(ctx, resource.WithAttributes(attribute.String("service.name", "light-prober"), attribute.String("service.version", serviceVersion)), resource.WithFromEnv())
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, errors.New("invalid OpenTelemetry resource configuration")
	}
	r := &recorder{provider: sdk.NewMeterProvider(sdk.WithResource(res), sdk.WithCardinalityLimit(64), sdk.WithReader(sdk.NewPeriodicReader(exporter, sdk.WithInterval(interval), sdk.WithTimeout(5*time.Second))),
		sdk.WithView(sdk.NewView(sdk.Instrument{Unit: "ms", Kind: sdk.InstrumentKindHistogram}, sdk.Stream{Aggregation: sdk.AggregationExplicitBucketHistogram{Boundaries: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000, 10000, 30000, 120000}}}))),
		traces: sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSampler(sampler), sdktrace.WithSpanLimits(sdktrace.SpanLimits{AttributeCountLimit: 16, AttributeValueLengthLimit: 128, EventCountLimit: 0, LinkCountLimit: 0}))}
	if (os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "") && os.Getenv("OTEL_TRACES_SAMPLER") != "always_off" {
		exp, e := otlptracehttp.New(ctx, otlptracehttp.WithCompression(otlptracehttp.GzipCompression), otlptracehttp.WithTimeout(5*time.Second))
		if e != nil {
			_ = r.provider.Shutdown(ctx)
			return nil, errors.New("cannot configure OTLP HTTP trace exporter")
		}
		r.traces.RegisterSpanProcessor(sdktrace.NewBatchSpanProcessor(exp, sdktrace.WithMaxQueueSize(256), sdktrace.WithMaxExportBatchSize(64), sdktrace.WithBatchTimeout(interval), sdktrace.WithExportTimeout(5*time.Second)))
	}
	r.tracer = r.traces.Tracer("light-prober")
	r.identity.Store("unassigned")
	meter := r.provider.Meter("light-prober")
	r.checks, _ = meter.Int64Counter("probe.checks", metric.WithUnit("{execution}"))
	r.checkDuration, _ = meter.Float64Histogram("probe.check.duration", metric.WithUnit("ms"))
	r.uploads, _ = meter.Int64Counter("probe.uploads", metric.WithUnit("{batch}"))
	r.uploadDuration, _ = meter.Float64Histogram("probe.upload.duration", metric.WithUnit("ms"))
	r.wireBytes, _ = meter.Int64Counter("probe.upload.bytes", metric.WithUnit("By"))
	r.configFailures, _ = meter.Int64Counter("probe.config.failures")
	r.operations, _ = meter.Int64Counter("probe.operations")
	r.operationDuration, _ = meter.Float64Histogram("probe.operation.duration", metric.WithUnit("ms"))
	gauges := map[string]metric.Int64ObservableGauge{}
	for _, name := range []string{"queue.results", "queue.bytes", "storage.bytes", "storage.limit", "checks.active", "runtime.gomaxprocs"} {
		gauges[name], _ = meter.Int64ObservableGauge("probe." + name)
	}
	age, _ := meter.Float64ObservableGauge("probe.queue.oldest.age", metric.WithUnit("s"))
	processCPU, _ := meter.Float64ObservableCounter("probe.process.cpu", metric.WithUnit("s"))
	processPeak, _ := meter.Int64ObservableGauge("probe.process.memory.peak", metric.WithUnit("By"))
	uptime, _ := meter.Float64ObservableGauge("probe.runtime.uptime", metric.WithUnit("s"))
	runtimeNames := []struct {
		path, name, unit string
		counter          bool
	}{
		{"/memory/classes/heap/objects:bytes", "heap", "By", false},
		{"/memory/classes/total:bytes", "memory", "By", false},
		{"/gc/heap/allocs:bytes", "allocations", "By", true},
		{"/gc/heap/frees:bytes", "frees", "By", true},
		{"/gc/cycles/total:gc-cycles", "gc.cycles", "{cycle}", true},
		{"/sched/goroutines:goroutines", "goroutines", "{goroutine}", false},
		{"/cpu/classes/gc/total:cpu-seconds", "gc.cpu", "s", true},
		{"/cpu/classes/user:cpu-seconds", "user.cpu", "s", true},
		{"/cpu/classes/scavenge/total:cpu-seconds", "scavenge.cpu", "s", true},
	}
	samples := make([]metrics.Sample, len(runtimeNames))
	ints := make([]metric.Int64Observable, len(runtimeNames))
	floats := make([]metric.Float64ObservableCounter, len(runtimeNames))
	instruments := []metric.Observable{age, uptime, processCPU, processPeak}
	for _, g := range gauges {
		instruments = append(instruments, g)
	}
	for i, v := range runtimeNames {
		samples[i].Name = v.path
		if strings.Contains(v.path, "cpu-seconds") {
			floats[i], _ = meter.Float64ObservableCounter("probe.runtime."+v.name, metric.WithUnit(v.unit))
			instruments = append(instruments, floats[i])
		} else if v.counter {
			ints[i], _ = meter.Int64ObservableCounter("probe.runtime."+v.name, metric.WithUnit(v.unit))
			instruments = append(instruments, ints[i])
		} else {
			ints[i], _ = meter.Int64ObservableGauge("probe.runtime."+v.name, metric.WithUnit(v.unit))
			instruments = append(instruments, ints[i])
		}
	}
	started := time.Now()
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		metrics.Read(samples)
		attrs := r.attrs()
		if user, system, peak, ok := processUsage(); ok {
			o.ObserveFloat64(processCPU, user, metric.WithAttributes(attribute.String("mode", "user"), attribute.String("probe.id", r.identity.Load().(string))))
			o.ObserveFloat64(processCPU, system, metric.WithAttributes(attribute.String("mode", "system"), attribute.String("probe.id", r.identity.Load().(string))))
			if peak > 0 {
				o.ObserveInt64(processPeak, peak, attrs)
			}
		}
		for i, sample := range samples {
			switch sample.Value.Kind() {
			case metrics.KindUint64:
				o.ObserveInt64(ints[i], int64(sample.Value.Uint64()), attrs)
			case metrics.KindFloat64:
				o.ObserveFloat64(floats[i], sample.Value.Float64(), attrs)
			}
		}
		values := map[string]int64{"queue.results": r.queued.Load(), "queue.bytes": r.queueBytes.Load(), "storage.bytes": r.databaseBytes.Load(), "storage.limit": r.databaseLimit.Load(), "checks.active": r.active.Load(), "runtime.gomaxprocs": int64(runtime.GOMAXPROCS(0))}
		for key, value := range values {
			o.ObserveInt64(gauges[key], value, attrs)
		}
		seconds := float64(0)
		if oldest := r.oldestAt.Load(); oldest > 0 {
			seconds = math.Max(0, float64(time.Now().Unix()-oldest))
		}
		o.ObserveFloat64(age, seconds, attrs)
		o.ObserveFloat64(uptime, time.Since(started).Seconds(), attrs)
		return nil
	}, instruments...)
	if err != nil {
		_ = r.Shutdown(ctx)
		return nil, err
	}
	return r, nil
}
func (r *recorder) Enabled() bool { return true }
func (r *recorder) attrs() metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("probe.id", r.identity.Load().(string)))
}
func (r *recorder) Identity(id string) {
	if id != "" {
		r.identity.Store(id)
	}
}
func (r *recorder) Start(ctx context.Context, name string) (context.Context, func(error)) {
	ctx, span := r.tracer.Start(ctx, name, trace.WithAttributes(attribute.String("probe.id", r.identity.Load().(string))))
	return ctx, func(err error) {
		if err != nil {
			span.SetStatus(codes.Error, "operation failed")
		}
		span.End()
	}
}
func (r *recorder) Inject(ctx context.Context, h http.Header) {
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(h))
}
func (r *recorder) Check(ctx context.Context, _ protocol.Result, method string) {
	// An execution is counted regardless of target availability. No per-target results enter telemetry.
	method = strings.ToUpper(method)
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT", "TCP_PING", "SSL_CERT", "ICMP_PING":
	default:
		method = "other"
	}
	attrs := metric.WithAttributes(attribute.String("method", method), attribute.String("probe.id", r.identity.Load().(string)))
	r.checks.Add(ctx, 1, attrs)
}
func (r *recorder) Upload(ctx context.Context, d time.Duration, bytes int, ok bool) {
	attrs := metric.WithAttributes(attribute.Bool("success", ok), attribute.String("probe.id", r.identity.Load().(string)))
	r.uploads.Add(ctx, 1, attrs)
	r.uploadDuration.Record(ctx, float64(d)/float64(time.Millisecond), attrs)
	r.wireBytes.Add(ctx, int64(bytes), attrs)
}
func (r *recorder) Operation(ctx context.Context, name string, d time.Duration, err error) {
	attrs := metric.WithAttributes(attribute.String("operation", name), attribute.Bool("success", err == nil), attribute.String("probe.id", r.identity.Load().(string)))
	r.operations.Add(ctx, 1, attrs)
	r.operationDuration.Record(ctx, float64(d)/float64(time.Millisecond), attrs)
	if name == "check.execute" {
		r.checkDuration.Record(ctx, float64(d)/float64(time.Millisecond), r.attrs())
	}
}
func (r *recorder) Queue(n uint64, b int64) { r.queued.Store(int64(n)); r.queueBytes.Store(b) }
func (r *recorder) Storage(bytes, limit, oldest int64) {
	r.databaseBytes.Store(bytes)
	r.databaseLimit.Store(limit)
	r.oldestAt.Store(oldest)
}
func (r *recorder) Active(delta int64)                { r.active.Add(delta) }
func (r *recorder) ConfigFailure(ctx context.Context) { r.configFailures.Add(ctx, 1, r.attrs()) }
func (r *recorder) Shutdown(ctx context.Context) error {
	return errors.Join(r.traces.Shutdown(ctx), r.provider.Shutdown(ctx))
}
