// Package client implements bounded, authenticated config and batch requests.
package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"light-prober/internal/protocol"
)

const maxResponse = 1 << 20

type Client struct {
	base     string
	token    string
	compress bool
	http     *http.Client
}

type HTTPError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return fmt.Sprintf("receiver returned HTTP %d", e.Status) }

func New(server, token string, compress, allowInsecure bool) (*Client, error) {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(allowInsecure && u.Scheme == "http")) {
		return nil, errors.New("server must be an HTTPS base URL without credentials, query, or fragment (HTTP requires --allow-insecure)")
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("LIGHT_PROBER_TOKEN is required and must not contain newlines")
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 2,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true,
	}
	return &Client{base: strings.TrimRight(server, "/"), token: token, compress: compress,
		http: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) request(ctx context.Context, method, path string, body []byte, compressed bool, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("cannot construct receiver request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "light-prober/1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if compressed {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("receiver connection failed; results remain on disk")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retry := time.Duration(0)
		if value := resp.Header.Get("Retry-After"); value != "" {
			if seconds, e := time.ParseDuration(value + "s"); e == nil {
				retry = seconds
			} else if until, e := http.ParseTime(value); e == nil {
				retry = time.Until(until)
			}
			if retry < 0 {
				retry = 0
			}
			if retry > time.Hour {
				retry = time.Hour
			}
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return &HTTPError{Status: resp.StatusCode, RetryAfter: retry}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return errors.New("invalid or oversized receiver response")
	}
	if err = json.Unmarshal(data, result); err != nil {
		return errors.New("receiver returned invalid JSON")
	}
	return nil
}

func ValidateConfig(cfg protocol.Config) error {
	if cfg.Version != protocol.Version || cfg.ProbeID == "" || len(cfg.ProbeID) > 128 || len(cfg.Monitors) > 100 {
		return errors.New("unsupported or oversized probe configuration")
	}
	seen := make(map[string]bool, len(cfg.Monitors))
	for _, m := range cfg.Monitors {
		if m.ID == "" || len(m.ID) > 128 || seen[m.ID] || len(m.Target) > 4096 || m.Target == "" || m.Timeout < 0 || m.Timeout > 120000 || len(m.Body) > 65536 || len(m.Headers) > 64 || len(m.ResponseKeyword) > 4096 || len(m.ResponseForbiddenKeyword) > 4096 {
			return errors.New("invalid monitor configuration")
		}
		seen[m.ID] = true
	}
	return nil
}

func (c *Client) Config(ctx context.Context) (protocol.Config, error) {
	var cfg protocol.Config
	err := c.request(ctx, "GET", "/api/probes/config", nil, false, &cfg)
	if err == nil {
		err = ValidateConfig(cfg)
	}
	return cfg, err
}

// Upload returns the actual wire bytes; it succeeds only for a matching durable acknowledgement.
func (c *Client) Upload(ctx context.Context, results []protocol.Result) (int, error) {
	if len(results) == 0 || len(results) > protocol.MaxBatchResults {
		return 0, errors.New("invalid batch size")
	}
	data, err := json.Marshal(results)
	if err != nil {
		return 0, err
	}
	hash := sha256.Sum256(data)
	batch := protocol.Batch{Version: protocol.Version, BatchID: hex.EncodeToString(hash[:]), Results: results}
	data, err = json.Marshal(batch)
	if err != nil {
		return 0, err
	}
	if len(data) > 512<<10 {
		return 0, errors.New("batch exceeds receiver limit")
	}
	if c.compress {
		var compressed bytes.Buffer
		writer, _ := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
		if _, err = writer.Write(data); err != nil {
			return 0, err
		}
		if err = writer.Close(); err != nil {
			return 0, err
		}
		data = compressed.Bytes()
	}
	var ack protocol.Ack
	err = c.request(ctx, "POST", "/api/probes/ingest", data, c.compress, &ack)
	if err != nil {
		return len(data), err
	}
	if ack.BatchID != batch.BatchID || ack.Accepted != len(results) {
		return len(data), errors.New("receiver acknowledgement mismatch; results remain on disk")
	}
	return len(data), nil
}
