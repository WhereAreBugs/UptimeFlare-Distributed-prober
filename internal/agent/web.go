package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"light-prober/internal/protocol"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type webMonitor struct {
	protocol.DisplayMonitor
	Latest *protocol.Result `json:"latest"`
}

func (a *Agent) displayMonitors() []protocol.DisplayMonitor {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.config.DisplayMonitors) > 0 {
		return append([]protocol.DisplayMonitor(nil), a.config.DisplayMonitors...)
	}
	result := make([]protocol.DisplayMonitor, 0, len(a.config.Monitors))
	for _, m := range a.config.Monitors {
		name := m.Name
		if name == "" {
			name = "未命名目标"
			if target, err := url.Parse(m.Target); err == nil && target.Hostname() != "" {
				name = target.Hostname()
			}
			if m.Method == "TCP_PING" {
				if host, _, err := net.SplitHostPort(m.Target); err == nil {
					name = host
				}
			}
		}
		result = append(result, protocol.DisplayMonitor{ID: m.ID, Name: name, Method: m.Method, IntervalSeconds: int(monitorInterval(m, a.opts.Interval) / time.Second), Timeout: m.Timeout})
	}
	return result
}
func (a *Agent) webHandler(loopback bool) http.Handler {
	files, _ := fs.Sub(webFiles, "web")
	static := http.FileServer(http.FS(files))
	expected := sha256.Sum256([]byte(a.opts.WebPassword))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if r.Method != "GET" {
			w.Header().Set("Allow", "GET")
			http.Error(w, "Method not allowed", 405)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if loopback && host != "localhost" && !net.ParseIP(host).IsLoopback() {
			http.Error(w, "Invalid host", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
			http.Error(w, "Invalid origin", 403)
			return
		}
		if a.opts.WebPassword != "" {
			_, password, ok := r.BasicAuth()
			actual := sha256.Sum256([]byte(password))
			if !ok || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="Probe", charset="UTF-8"`)
				http.Error(w, "Authentication required", 401)
				return
			}
		}
		send := func(value any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(value)
		}
		switch r.URL.Path {
		case "/api/status":
			value, err := a.status()
			if err != nil {
				http.Error(w, "Storage unavailable", 503)
				return
			}
			a.mu.RLock()
			probe := a.config.Probe
			registered := a.identity != ""
			a.mu.RUnlock()
			send(struct {
				Status     localStatus           `json:"status"`
				Probe      protocol.Registration `json:"probe"`
				Registered bool                  `json:"registered"`
			}{value, probe, registered})
		case "/api/monitors":
			page, err := strconv.Atoi(r.URL.Query().Get("page"))
			if err != nil || page < 1 {
				page = 1
			}
			monitors := a.displayMonitors()
			start := min((min(page, 51)-1)*10, len(monitors))
			end := min(start+10, len(monitors))
			rows := make([]webMonitor, 0, end-start)
			for _, m := range monitors[start:end] {
				latest, err := a.queue.Latest(m.ID)
				if err != nil {
					http.Error(w, "Storage unavailable", 503)
					return
				}
				rows = append(rows, webMonitor{m, latest})
			}
			send(struct {
				Monitors []webMonitor `json:"monitors"`
				Total    int          `json:"total"`
			}{rows, len(monitors)})
		case "/api/history":
			id := r.URL.Query().Get("monitor")
			found := false
			for _, m := range a.displayMonitors() {
				if m.ID == id {
					found = true
					break
				}
			}
			if !found {
				http.NotFound(w, r)
				return
			}
			history, err := a.queue.History(id, time.Now().Unix())
			if err != nil {
				http.Error(w, "Storage unavailable", 503)
				return
			}
			send(history)
		default:
			if r.URL.Path != "/" && r.URL.Path != "/app.js" && r.URL.Path != "/style.css" {
				http.NotFound(w, r)
				return
			}
			static.ServeHTTP(w, r)
		}
	})
}
func (a *Agent) startWeb(ctx context.Context) (*http.Server, error) {
	if a.opts.WebListen == "" {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(a.opts.WebListen)
	if err != nil {
		return nil, err
	}
	// Require a literal address: DNS changes must not turn a local listener public.
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, errors.New("web-listen requires a literal IP address")
	}
	loopback := ip.IsLoopback()
	if !loopback && len(a.opts.WebPassword) < 16 {
		return nil, errors.New("non-loopback dashboard requires LIGHT_PROBER_WEB_PASSWORD of at least 16 characters")
	}
	listener, err := net.Listen("tcp", a.opts.WebListen)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: a.webHandler(loopback), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("dashboard stopped", "error", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	a.log.Info("local dashboard listening", "address", listener.Addr().String())
	return server, nil
}
