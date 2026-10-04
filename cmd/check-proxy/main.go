// check-proxy runs authenticated HTTP delegation for HTTP, TCP, TLS and ICMP checks.
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"light-prober/internal/checkproxy"
)

func main() {
	if err := run(); err != nil {
		slog.Error("check proxy stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:8081", "HTTP listening address")
	location := flag.String("location", "configured", "execution location label returned to callers")
	concurrency := flag.Int("concurrency", 4, "maximum simultaneous checks (1..32)")
	maxTimeout := flag.Duration("max-timeout", 120*time.Second, "maximum accepted target timeout (1ms..120s)")
	cert := flag.String("tls-cert", "", "optional HTTPS certificate file")
	key := flag.String("tls-key", "", "optional HTTPS private key file")
	ca := flag.String("ca-file", "", "optional PEM trust roots appended to system roots for outgoing checks")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if (*cert == "") != (*key == "") {
		return errors.New("tls-cert and tls-key must be supplied together")
	}
	var roots *x509.CertPool
	if *ca != "" {
		file, err := os.Open(*ca)
		if err != nil {
			return errors.New("cannot read ca-file")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		_ = file.Close()
		if readErr != nil || len(data) > 1<<20 {
			return errors.New("ca-file is unreadable or exceeds 1 MiB")
		}
		roots, err = x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(data) {
			return errors.New("ca-file contains no valid certificates")
		}
	}
	handler, err := checkproxy.New(checkproxy.Options{Token: os.Getenv("LIGHT_PROBER_CHECK_PROXY_TOKEN"), Location: *location, Concurrency: *concurrency, MaxTimeout: *maxTimeout, RootCAs: roots})
	if err != nil {
		return err
	}
	defer handler.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: *maxTimeout + 15*time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	serveErr := make(chan error, 1)
	go func() {
		if *cert != "" {
			serveErr <- server.ListenAndServeTLS(*cert, *key)
		} else {
			serveErr <- server.ListenAndServe()
		}
	}()
	slog.Info("check proxy listening", "listen", *listen, "location", *location, "concurrency", *concurrency, "tls", *cert != "")
	select {
	case err = <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("HTTP listener failed: %w", err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = server.Shutdown(shutdown); err != nil {
		_ = server.Close()
		return err
	}
	return nil
}
