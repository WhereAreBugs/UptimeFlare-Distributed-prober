package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"light-prober/internal/agent"
	"light-prober/internal/telemetry"
)

var version = "dev"

func run() error {
	base, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	opts := agent.Options{}
	flag.StringVar(&opts.Server, "server", os.Getenv("LIGHT_PROBER_SERVER"), "UptimeFlare base URL (or LIGHT_PROBER_SERVER)")
	flag.StringVar(&opts.DataDir, "data-dir", filepath.Join(base, "light-prober"), "persistent local data directory")
	flag.DurationVar(&opts.Interval, "interval", time.Minute, "check interval; overlapping rounds are skipped")
	flag.DurationVar(&opts.FlushInterval, "flush-interval", 5*time.Minute, "batch upload interval")
	flag.DurationVar(&opts.ConfigInterval, "config-interval", 5*time.Minute, "configuration refresh interval")
	flag.IntVar(&opts.Concurrency, "concurrency", 4, "maximum concurrent target checks (1..32)")
	maxMiB := flag.Int64("max-queue-mib", 256, "maximum unacknowledged payload bytes; full queue stops checks")
	flag.BoolVar(&opts.Compress, "gzip", true, "compress HTTP batch requests with gzip")
	flag.BoolVar(&opts.AllowInsecure, "allow-insecure", false, "allow HTTP receiver for local testing")
	otelEnabled := flag.Bool("telemetry", false, "enable OpenTelemetry OTLP HTTP metrics (OTEL_EXPORTER_OTLP_* env)")
	otelInterval := flag.Duration("telemetry-interval", time.Minute, "metric export interval (minimum 10s)")
	printVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *printVersion {
		fmt.Println(version)
		return nil
	}
	if *maxMiB < 1 || *maxMiB > 1024*1024 {
		return fmt.Errorf("max-queue-mib must be 1..1048576")
	}
	opts.MaxQueueBytes = *maxMiB << 20
	opts.Token = os.Getenv("LIGHT_PROBER_TOKEN")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	metrics, err := telemetry.New(ctx, *otelEnabled, *otelInterval)
	if err != nil {
		return err
	}
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = metrics.Shutdown(shutdown)
	}()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	a, err := agent.New(opts, metrics, log)
	if err != nil {
		return err
	}
	defer a.Close()
	log.Info("probe started", "version", version, "interval", opts.Interval, "flush_interval", opts.FlushInterval, "concurrency", opts.Concurrency, "telemetry", *otelEnabled)
	return a.Run(ctx)
}
func main() {
	if err := run(); err != nil {
		slog.Error("probe stopped", "error", err)
		os.Exit(1)
	}
}
