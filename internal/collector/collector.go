// Package collector turns FRITZ!Box data into Prometheus metrics.
//
// The Exporter is the single prometheus.Collector registered in main.go.
// On every scrape it runs all enabled Scrapers (device, wan, wlan, ...)
// in parallel and adds metrics about the scrape itself.
package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/juvradev/fritzbox_exporter/internal/tr064"
	"github.com/juvradev/fritzbox_exporter/internal/webui"
)

// namespace is the prefix of every metric name: fritzbox_*.
const namespace = "fritzbox"

// DefaultAuthBackoff is how long a collector pauses after the box rejected
// the credentials. Retrying on every scrape would spam the logs and can make
// the box block the user for longer and longer.
const DefaultAuthBackoff = 5 * time.Minute

// Caller is the part of tr064.Client the collectors need. Using an interface
// lets tests replace the real box with canned responses.
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
	scrapers    []Scraper
	states      []*scraperState // one per scraper, same order
	timeout     time.Duration
	authBackoff time.Duration
	now         func() time.Time // replaceable in tests
	logger      *slog.Logger

	up             *prometheus.Desc
	scrapeSuccess  *prometheus.Desc
	scrapeDuration *prometheus.Desc
}

// scraperState remembers the outcome of the previous scrape, so failures are
// logged once when they start (not on every scrape) and authentication
// errors can pause the scraper. Parallel /metrics requests share it.
type scraperState struct {
	mu          sync.Mutex
	failing     bool
	pausedUntil time.Time
}

// NewExporter creates an Exporter. timeout limits one complete scrape.
func NewExporter(scrapers []Scraper, timeout time.Duration, logger *slog.Logger) *Exporter {
	states := make([]*scraperState, len(scrapers))
	for i := range states {
		states[i] = &scraperState{}
	}

	return &Exporter{
		scrapers:    scrapers,
		states:      states,
		timeout:     timeout,
		authBackoff: DefaultAuthBackoff,
		now:         time.Now,
		logger:      logger,
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
	for i, s := range e.scrapers {
		// The scrapers are independent, so they run in parallel; the scrape
		// takes as long as the slowest one instead of the sum of all.
		wg.Go(func() {
			ok, duration := e.runScraper(ctx, s, e.states[i], ch)
			if ok {
				anyAlive.Store(true)
			}
			ch <- prometheus.MustNewConstMetric(e.scrapeSuccess, prometheus.GaugeValue, boolToFloat(ok), s.Name())
			ch <- prometheus.MustNewConstMetric(e.scrapeDuration, prometheus.GaugeValue, duration, s.Name())
		})
	}
	wg.Wait()

	ch <- prometheus.MustNewConstMetric(e.up, prometheus.GaugeValue, boolToFloat(anyAlive.Load()))
}

// runScraper runs one scraper unless it is paused, and records the outcome.
// It returns whether the scraper succeeded and how long it took in seconds.
func (e *Exporter) runScraper(ctx context.Context, s Scraper, st *scraperState, ch chan<- prometheus.Metric) (bool, float64) {
	st.mu.Lock()
	pausedUntil := st.pausedUntil
	st.mu.Unlock()

	if now := e.now(); now.Before(pausedUntil) {
		e.logger.Debug("collector paused after authentication error",
			"collector", s.Name(), "remaining", pausedUntil.Sub(now).Round(time.Second).String())
		return false, 0
	}

	start := time.Now()
	err := s.Scrape(ctx, ch)
	duration := time.Since(start).Seconds()

	e.record(s.Name(), st, err, duration)
	return err == nil, duration
}

// record updates the scraper state and logs only on changes: the first
// failure, a recovery, or a new pause. Repeated identical failures are
// logged at debug level, so an unreachable box does not flood the logs.
func (e *Exporter) record(name string, st *scraperState, err error, duration float64) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if err == nil {
		if st.failing {
			e.logger.Info("collector recovered", "collector", name, "duration_seconds", duration)
		} else {
			e.logger.Debug("collector succeeded", "collector", name, "duration_seconds", duration)
		}
		st.failing = false
		return
	}

	pause := e.pauseFor(err)
	switch {
	case pause > 0:
		st.pausedUntil = e.now().Add(pause)
		e.logger.Warn("collector failed, pausing it", "collector", name,
			"paused_for", pause.String(), "err", err)
	case !st.failing:
		e.logger.Warn("collector failed", "collector", name, "duration_seconds", duration, "err", err)
	default:
		e.logger.Debug("collector still failing", "collector", name, "duration_seconds", duration, "err", err)
	}
	st.failing = true
}

// pauseFor returns how long a scraper should pause after err, or 0 if it
// should simply try again on the next scrape.
func (e *Exporter) pauseFor(err error) time.Duration {
	var blocked *webui.BlockedError
	switch {
	case errors.As(err, &blocked):
		// The box told us how long it refuses logins.
		return time.Duration(blocked.Seconds) * time.Second
	case errors.Is(err, tr064.ErrUnauthorized), errors.Is(err, webui.ErrUnauthorized):
		return e.authBackoff
	default:
		return 0
	}
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
