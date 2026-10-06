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
	"light-prober/internal/protocol"
	"light-prober/internal/telemetry"
)

var version = "dev"

func run() error {
	opts := agent.Options{}
	opts.Version = version
	flag.StringVar(&opts.WebListen, "web-listen", "127.0.0.1:9187", "read-only local dashboard address; empty disables it")
	opts.WebPassword = os.Getenv("LIGHT_PROBER_WEB_PASSWORD")
	flag.StringVar(&opts.Server, "server", os.Getenv("LIGHT_PROBER_SERVER"), "UptimeFlare base URL (or LIGHT_PROBER_SERVER)")
	flag.StringVar(&opts.DataDir, "data-dir", "", "persistent local data directory (default: user config directory/light-prober)")
	flag.DurationVar(&opts.Interval, "interval", protocol.DefaultIntervalSeconds*time.Second, "legacy fallback check interval; overridden by each remote monitor's intervalSeconds")
	flag.DurationVar(&opts.FlushInterval, "flush-interval", 5*time.Minute, "batch upload interval (capped at the minimum monitor interval)")
	flag.DurationVar(&opts.ConfigInterval, "config-interval", 5*time.Minute, "configuration refresh interval")
	flag.IntVar(&opts.Concurrency, "concurrency", 4, "maximum concurrent target checks (1..32)")
	maxMiB := flag.Int64("max-queue-mib", 1024, "maximum pending payload MiB (1..1024); database including overhead capped at 1 GiB")
	flag.BoolVar(&opts.Compress, "gzip", true, "compress HTTP batch requests with gzip")
	flag.BoolVar(&opts.AllowInsecure, "allow-insecure", false, "allow HTTP receiver for local testing")
	otelEnabled := flag.Bool("telemetry", false, "enable OpenTelemetry runtime metrics and sampled traces (OTEL_EXPORTER_OTLP_* env)")
	otelInterval := flag.Duration("telemetry-interval", time.Minute, "metric export interval (minimum 10s)")
	printVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *printVersion {
		fmt.Println(version)
		return nil
	}
	var err error
	opts.DataDir, err = resolveDataDirectory(opts.DataDir)
	if err != nil {
		return err
	}
	if *maxMiB < 1 || *maxMiB > 1024 {
		return fmt.Errorf("max-queue-mib must be 1..1024")
	}
	opts.MaxQueueBytes = *maxMiB << 20
	opts.Token = os.Getenv("LIGHT_PROBER_TOKEN")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	metrics, err := telemetry.New(ctx, *otelEnabled, *otelInterval, version)
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
	log.Info("probe started", "version", version, "legacy_interval", opts.Interval, "max_flush_interval", opts.FlushInterval, "concurrency", opts.Concurrency, "telemetry", *otelEnabled)
	return a.Run(ctx)
}

func resolveDataDirectory(configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "light-prober"), nil
}

func main() {
	if err := run(); err != nil {
		slog.Error("probe stopped", "error", err)
		os.Exit(1)
	}
}
