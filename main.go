// Command fritzbox_exporter exposes FRITZ!Box metrics for Prometheus.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/juvradev/fritzbox_exporter/internal/collector"
	"github.com/juvradev/fritzbox_exporter/internal/config"
	"github.com/juvradev/fritzbox_exporter/internal/server"
	"github.com/juvradev/fritzbox_exporter/internal/tr064"
)

// version is set at build time:
//
//	go build -ldflags "-X main.version=v0.1.0"
var version = "dev"

// shutdownTimeout is how long in-flight scrapes may take after SIGTERM.
// Kubernetes waits 30s (terminationGracePeriodSeconds) before it kills the pod.
const shutdownTimeout = 10 * time.Second

func main() {
	// The level can be changed after the config is loaded. Until then
	// everything at info and above is logged.
	level := new(slog.LevelVar)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if err := run(logger, level); err != nil {
		logger.Error("exporter stopped with error", "err", err)
		os.Exit(1)
	}
}

// run contains the actual program. Keeping it separate from main means
// every error path ends in one place and deferred calls still run.
func run(logger *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level.Set(cfg.LogLevel)
	logger.Info("starting fritzbox_exporter", "version", version, "config", cfg)

	// ctx is cancelled on Ctrl+C (SIGINT) or when Kubernetes stops the pod (SIGTERM).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A dedicated registry instead of the global default one: only what we
	// register here shows up on /metrics.
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),                                       // go_* (goroutines, GC, memory)
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), // process_* (CPU, RSS, fds)
		collectors.NewBuildInfoCollector(),                                // go_build_info
	)

	// One TR-064 client is shared by all collectors.
	client := tr064.NewClient(cfg.FritzBoxURL, cfg.Username, cfg.Password, cfg.ScrapeTimeout)
	registry.MustRegister(collector.NewExporter(scrapers(cfg, client, logger), cfg.ScrapeTimeout, logger))

	srv := server.New(cfg.ListenAddress, registry, logger)
	return srv.Run(ctx, shutdownTimeout)
}

// scrapers builds the enabled collectors in the order given in FRITZBOX_COLLECTORS.
func scrapers(cfg config.Config, client collector.Caller, logger *slog.Logger) []collector.Scraper {
	var list []collector.Scraper
	for _, name := range cfg.Collectors {
		switch name {
		case "device":
			list = append(list, collector.NewDevice(client))
		case "wan":
			list = append(list, collector.NewWAN(client))
		case "wlan":
			list = append(list, collector.NewWLAN(client, collector.DefaultWLANInterfaces))
		default:
			// "docsis" is accepted by the config but implemented in M5.
			logger.Warn("collector not implemented yet, skipping", "collector", name)
		}
	}
	return list
}
