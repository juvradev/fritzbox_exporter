// Package collector turns FRITZ!Box data into Prometheus metrics.
//
// The Exporter is the single prometheus.Collector registered in main.go.
// On every scrape it runs all enabled Scrapers (device, wan, wlan, ...)
// in parallel and adds metrics about the scrape itself.
package collector

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/juvradev/fritzbox_exporter/internal/tr064"
)

// namespace is the prefix of every metric name: fritzbox_*.
const namespace = "fritzbox"

// Caller is the part of tr064.Client the collectors need. Using an interface
type Caller interface {
	Call(ctx context.Context, svc tr064.Service, action string, args map[string]string) (map[string]string, error)
}

// Scraper collects one group of metrics, e.g. everything about the WAN link.
type Scraper interface {
	// Name is used in the "collector" label and in log messages.
	Name() string
	// Describe sends the descriptors of all metrics the scraper can emit.
	Describe(ch chan<- *prometheus.Desc)
	// Scrape queries the box and sends the metrics. Metrics sent before an
	// error are kept, so a partial result is still visible.
	Scrape(ctx context.Context, ch chan<- prometheus.Metric) error
}

// Exporter runs all scrapers on every Prometheus scrape.
type Exporter struct {
	scrapers []Scraper
	timeout  time.Duration
	logger   *slog.Logger

	up             *prometheus.Desc
	scrapeSuccess  *prometheus.Desc
	scrapeDuration *prometheus.Desc
}

// NewExporter creates an Exporter. timeout limits one complete scrape.
func NewExporter(scrapers []Scraper, timeout time.Duration, logger *slog.Logger) *Exporter {
	return &Exporter{
		scrapers: scrapers,
		timeout:  timeout,
		logger:   logger,
		up: prometheus.NewDesc(
			namespace+"_up",
			"Whether the FRITZ!Box could be reached (1 if at least one collector succeeded).",
			nil, nil,
		),
		scrapeSuccess: prometheus.NewDesc(
			namespace+"_scrape_collector_success",
			"Whether the collector succeeded during the last scrape.",
			[]string{"collector"}, nil,
		),
		scrapeDuration: prometheus.NewDesc(
			namespace+"_scrape_collector_duration_seconds",
			"How long the collector took during the last scrape.",
			[]string{"collector"}, nil,
		),
	}
}

// Describe implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.up
	ch <- e.scrapeSuccess
	ch <- e.scrapeDuration
	for _, s := range e.scrapers {
		s.Describe(ch)
	}
}

// Collect implements prometheus.Collector. It is called on every request
// to /metrics.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
	defer cancel()

	var (
		wg       sync.WaitGroup
		anyAlive atomic.Bool
	)
	for _, s := range e.scrapers {
		// The scrapers are independent, so they run in parallel; the scrape
		// takes as long as the slowest one instead of the sum of all.
		wg.Go(func() {
			start := time.Now()
			err := s.Scrape(ctx, ch)
			duration := time.Since(start).Seconds()

			success := 1.0
			if err != nil {
				success = 0
				e.logger.Warn("collector failed", "collector", s.Name(), "duration_seconds", duration, "err", err)
			} else {
				anyAlive.Store(true)
				e.logger.Debug("collector succeeded", "collector", s.Name(), "duration_seconds", duration)
			}

			ch <- prometheus.MustNewConstMetric(e.scrapeSuccess, prometheus.GaugeValue, success, s.Name())
			ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, duration, s.Name())
		})
	}
	wg.Wait()

	ch <- prometheus.MustNewConstMetric(e.up, prometheus.GaugeValue, boolToFloat(anyAlive.Load()))
}

// parseFloat reads a numeric response field, e.g. "NewUpTime": "2518554".
func parseFloat(values map[string]string, key string) (float64, error) {
	raw, ok := values[key]
	if !ok {
		return 0, fmt.Errorf("response has no field %s", key)
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("field %s: %w", key, err)
	}
	return v, nil
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
