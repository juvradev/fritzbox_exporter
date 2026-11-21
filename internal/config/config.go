// Package config loads the exporter configuration from environment variables.
//
// All settings come from the environment (12-factor style), so the same
// image runs unchanged locally, in Docker and in Kubernetes, where the values
// are set by the HelmRelease and the password comes from a Secret.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// Environment variable names.
const (
	EnvFritzBoxURL      = "FRITZBOX_URL"
	EnvFritzBoxUsername = "FRITZBOX_USERNAME"
	EnvFritzBoxPassword = "FRITZBOX_PASSWORD"
	EnvCollectors       = "FRITZBOX_COLLECTORS"
	EnvListenAddress    = "LISTEN_ADDRESS"
	EnvLogLevel         = "LOG_LEVEL"
	EnvScrapeTimeout    = "SCRAPE_TIMEOUT"
	EnvWebUIURL         = "FRITZBOX_WEBUI_URL"
)

// Defaults for optional settings.
const (
	DefaultFritzBoxURL   = "http://fritz.box:49000"
	DefaultCollectors    = "device,wan,wlan,docsis"
	DefaultListenAddress = ":9133"
	DefaultLogLevel      = "info"
	DefaultScrapeTimeout = 10 * time.Second
)

// KnownCollectors lists every collector name accepted in FRITZBOX_COLLECTORS.
var KnownCollectors = []string{"device", "wan", "wlan", "docsis"}

// Config is the complete exporter configuration.
type Config struct {
	FritzBoxURL   string        // TR-064 base URL, e.g. http://10.10.0.1:49000
	Username      string        // FRITZ!Box user with the "FRITZ!Box settings" right
	Password      string        // never log this; see LogValue
	Collectors    []string      // enabled collectors, e.g. ["device", "wan"]
	ListenAddress string        // address for the HTTP server, e.g. ":9133"
	LogLevel      slog.Level    // minimum level for log output
	ScrapeTimeout time.Duration // upper bound for one call to the box
	WebUIURL      string        // web UI base URL for DOCSIS data, e.g. http://10.10.0.1
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

// lookupFunc has the signature of os.LookupEnv. Tests pass a fake one,
// so they do not touch the real environment.
type lookupFunc func(key string) (string, bool)

func load(lookup lookupFunc) (Config, error) {
	// Collect all problems instead of stopping at the first one, so a broken
	// deployment shows every missing variable in a single log line.
	var errs []error

	get := func(key, fallback string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return fallback
	}
	require := func(key string) string {
		v := get(key, "")
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
		return v
	}

	cfg := Config{
		FritzBoxURL:   strings.TrimRight(get(EnvFritzBoxURL, DefaultFritzBoxURL), "/"),
		Username:      require(EnvFritzBoxUsername),
		ListenAddress: get(EnvListenAddress, DefaultListenAddress),
	}

	// The password is not trimmed: spaces may be part of it.
	if v, ok := lookup(EnvFritzBoxPassword); ok && v != "" {
		cfg.Password = v
	} else {
		errs = append(errs, fmt.Errorf("%s is required", EnvFritzBoxPassword))
	}

	if err := validateURL(cfg.FritzBoxURL); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvFritzBoxURL, err))
	} else {
		// The web UI runs on the same host as TR-064, but on the default port.
		cfg.WebUIURL = strings.TrimRight(get(EnvWebUIURL, webUIFromTR064(cfg.FritzBoxURL)), "/")
		if err := validateURL(cfg.WebUIURL); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvWebUIURL, err))
		}
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(get(EnvLogLevel, DefaultLogLevel))); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvLogLevel, err))
	}

	timeout, err := time.ParseDuration(get(EnvScrapeTimeout, DefaultScrapeTimeout.String()))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("%s: %w", EnvScrapeTimeout, err))
	case timeout <= 0:
		errs = append(errs, fmt.Errorf("%s must be positive, got %s", EnvScrapeTimeout, timeout))
	default:
		cfg.ScrapeTimeout = timeout
	}

	collectors, err := parseCollectors(get(EnvCollectors, DefaultCollectors))
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvCollectors, err))
	}
	cfg.Collectors = collectors

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// CollectorEnabled reports whether the named collector is switched on.
func (c Config) CollectorEnabled(name string) bool {
	return slices.Contains(c.Collectors, name)
}

// LogValue implements slog.LogValuer. It controls how the config appears in
// logs and makes sure the password is never written out.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("fritzbox_url", c.FritzBoxURL),
		slog.String("username", c.Username),
		slog.String("password", "[REDACTED]"),
		slog.Any("collectors", c.Collectors),
		slog.String("listen_address", c.ListenAddress),
		slog.String("log_level", c.LogLevel.String()),
		slog.String("scrape_timeout", c.ScrapeTimeout.String()), // "10s" instead of nanoseconds
		slog.String("webui_url", c.WebUIURL),
	)
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host in %q", raw)
	}
	return nil
}

// webUIFromTR064 derives the web UI URL from the TR-064 URL by dropping the
// port: http://192.168.178.1:49000 -> http://192.168.178.1.
// The input must already be a valid URL.
func webUIFromTR064(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Scheme + "://" + u.Hostname()
}

// parseCollectors turns "wan, wlan" into ["wan", "wlan"] and rejects
// unknown or duplicate names.
func parseCollectors(raw string) ([]string, error) {
	var names []string
	for name := range strings.SplitSeq(raw, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		switch {
		case name == "":
			continue
		case !slices.Contains(KnownCollectors, name):
			return nil, fmt.Errorf("unknown collector %q (known: %s)", name, strings.Join(KnownCollectors, ", "))
		case slices.Contains(names, name):
			return nil, fmt.Errorf("collector %q listed twice", name)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New("at least one collector must be enabled")
	}
	return names, nil
}
