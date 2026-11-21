package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func newTestServer(t *testing.T) (*Server, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	logger := slog.New(slog.DiscardHandler)
	return New("127.0.0.1:0", registry, logger), registry
}

// get sends a request straight to the handler, without opening a port.
func get(t *testing.T, s *Server, method, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	body, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(body)
}

func TestHealthz(t *testing.T) {
	s, _ := newTestServer(t)
	if code, _ := get(t, s, http.MethodGet, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
	}
}

func TestReadyzFollowsState(t *testing.T) {
	s, _ := newTestServer(t)

	if code, _ := get(t, s, http.MethodGet, "/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz before start = %d, want 503", code)
	}

	s.ready.Store(true)
	if code, _ := get(t, s, http.MethodGet, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz while ready = %d, want 200", code)
	}
}

func TestMetricsExposesRegistry(t *testing.T) {
	s, registry := newTestServer(t)

	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_value", Help: "Test gauge."})
	g.Set(42)
	registry.MustRegister(g)

	code, body := get(t, s, http.MethodGet, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", code)
	}
	if !strings.Contains(body, "test_value 42") {
		t.Errorf("/metrics misses test_value:\n%s", body)
	}
}

func TestRoutes(t *testing.T) {
	s, _ := newTestServer(t)

	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/unknown", http.StatusNotFound},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		if code, _ := get(t, s, tt.method, tt.path); code != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, code, tt.want)
		}
	}
}

func TestRunShutsDownGracefully(t *testing.T) {
	s, _ := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, time.Second) }()

	// Wait until the server is listening.
	deadline := time.Now().Add(2 * time.Second)
	for !s.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("server did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel() // simulates SIGTERM

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after cancel")
	}
	if s.Ready() {
		t.Error("server still ready after shutdown")
	}
}

func TestRunFailsOnBusyPort(t *testing.T) {
	busy := httptest.NewServer(http.NotFoundHandler())
	defer busy.Close()

	s := New(strings.TrimPrefix(busy.URL, "http://"), prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err := s.Run(context.Background(), time.Second); err == nil {
		t.Fatal("Run() on a busy port returned nil, want error")
	}
}
