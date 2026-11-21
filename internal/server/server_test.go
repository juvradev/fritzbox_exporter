package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func newTestServer(t *testing.T) (*Server, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	logger := slog.New(slog.DiscardHandler)
	return New("127.0.0.1:0", registry, logger, 5*time.Second), registry
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

	s := New(strings.TrimPrefix(busy.URL, "http://"), prometheus.NewRegistry(), slog.New(slog.DiscardHandler), time.Second)
	if err := s.Run(context.Background(), time.Second); err == nil {
		t.Fatal("Run() on a busy port returned nil, want error")
	}
}

// brokenCollector emits one invalid metric, like a collector with a bug.
type brokenCollector struct{ desc *prometheus.Desc }

func (c brokenCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }
func (c brokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(c.desc, errors.New("broken"))
}

func TestMetricsContinuesOnCollectorError(t *testing.T) {
	s, registry := newTestServer(t)

	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_value", Help: "Test gauge."})
	g.Set(42)
	registry.MustRegister(g, brokenCollector{
		desc: prometheus.NewDesc("test_broken", "Broken metric.", nil, nil),
	})

	code, body := get(t, s, http.MethodGet, "/metrics")
	if code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200 despite the broken collector", code)
	}
	if !strings.Contains(body, "test_value 42") {
		t.Errorf("/metrics misses the healthy metric:\n%s", body)
	}
}

// blockingCollector blocks in Collect until release is closed.
type blockingCollector struct {
	desc    *prometheus.Desc
	entered chan struct{}
	release chan struct{}
}

func (c blockingCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }
func (c blockingCollector) Collect(ch chan<- prometheus.Metric) {
	c.entered <- struct{}{}
	<-c.release
	ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, 1)
}

func TestMetricsLimitsRequestsInFlight(t *testing.T) {
	s, registry := newTestServer(t)
	blocker := blockingCollector{
		desc:    prometheus.NewDesc("test_slow", "Slow metric.", nil, nil),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	registry.MustRegister(blocker)

	// Fill all slots with scrapes that hang in Collect.
	var wg sync.WaitGroup
	for range maxMetricsRequestsInFlight {
		wg.Go(func() { get(t, s, http.MethodGet, "/metrics") })
		<-blocker.entered
	}

	// One more request must be rejected instead of querying the box again.
	if code, _ := get(t, s, http.MethodGet, "/metrics"); code != http.StatusServiceUnavailable {
		t.Errorf("extra /metrics request = %d, want 503", code)
	}

	close(blocker.release)
	wg.Wait()
}
