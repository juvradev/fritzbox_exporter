// Package server provides the exporter's HTTP server with the endpoints
// Kubernetes and Prometheus need: /metrics, /healthz and /readyz.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server is an HTTP server with health endpoints and graceful shutdown.
type Server struct {
	http   *http.Server
	logger *slog.Logger
	ready  atomic.Bool // read by /readyz, written by Run
}

// New creates a server that listens on addr and serves the metrics
// from registry.
func New(addr string, registry *prometheus.Registry, logger *slog.Logger) *Server {
	s := &Server{logger: logger}
	s.http = &http.Server{
		Addr:    addr,
		Handler: s.routes(registry),
		// Protects against clients that open a connection and never send
		// headers (Slowloris). Required by the gosec linter as well.
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// routes registers all endpoints. Go 1.22+ patterns like "GET /healthz"
// match the method as well, so a POST gets 405 automatically.
func (s *Server) routes(registry *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		// Log errors that happen while collecting or encoding metrics.
		ErrorLog: slog.NewLogLogger(s.logger.Handler(), slog.LevelError),
		// Also expose promhttp_metric_handler_* metrics about /metrics itself.
		Registry: registry,
	}))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /{$}", handleIndex) // "{$}" matches only "/" itself

	return mux
}

// handleHealthz is the liveness probe: if the process can answer, it is alive.
// It deliberately does not check the FRITZ!Box; an unreachable box must not
// make Kubernetes restart the exporter (that is what fritzbox_up is for).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "ok")
}

// handleReadyz is the readiness probe: 200 while serving, 503 during shutdown
// so Kubernetes stops sending new traffic to this pod.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		writeText(w, http.StatusServiceUnavailable, "not ready")
		return
	}
	writeText(w, http.StatusOK, "ready")
}

func handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, `<html><head><title>FRITZ!Box Exporter</title></head><body>
<h1>FRITZ!Box Exporter</h1>
<p><a href="/metrics">Metrics</a> · <a href="/healthz">Health</a> · <a href="/readyz">Ready</a></p>
</body></html>`)
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, body)
}

// Ready reports whether the server is accepting requests.
func (s *Server) Ready() bool {
	return s.ready.Load()
}

// Run starts the server and blocks until ctx is cancelled (e.g. on SIGTERM).
// It then marks the server as not ready and shuts it down gracefully, giving
// in-flight requests up to shutdownTimeout to finish.
func (s *Server) Run(ctx context.Context, shutdownTimeout time.Duration) error {
	// Listen first, so a port conflict is reported immediately as an error
	// and the server only becomes ready once it can actually accept requests.
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.http.Addr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.http.Serve(ln)
	}()

	s.ready.Store(true)
	s.logger.Info("http server started", "address", ln.Addr().String())

	select {
	case err := <-serveErr:
		// Serve returned without a shutdown request: a real error.
		s.ready.Store(false)
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	s.ready.Store(false)
	s.logger.Info("shutting down http server", "timeout", shutdownTimeout.String())

	// ctx is already cancelled, so the shutdown needs a fresh context.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	// After Shutdown, Serve returns http.ErrServerClosed, which is expected.
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}

	s.logger.Info("http server stopped")
	return nil
}
