package config

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

// env builds a fake os.LookupEnv from a map.
func env(vars map[string]string) lookupFunc {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(env(map[string]string{
		EnvFritzBoxUsername: "prometheus",
		EnvFritzBoxPassword: "secret",
	}))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.FritzBoxURL != DefaultFritzBoxURL {
		t.Errorf("FritzBoxURL = %q, want %q", cfg.FritzBoxURL, DefaultFritzBoxURL)
	}
	if cfg.ListenAddress != DefaultListenAddress {
		t.Errorf("ListenAddress = %q, want %q", cfg.ListenAddress, DefaultListenAddress)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
	}
	if cfg.ScrapeTimeout != DefaultScrapeTimeout {
		t.Errorf("ScrapeTimeout = %v, want %v", cfg.ScrapeTimeout, DefaultScrapeTimeout)
	}
	if want := []string{"device", "wan", "wlan"}; !slices.Equal(cfg.Collectors, want) {
		t.Errorf("Collectors = %v, want %v", cfg.Collectors, want)
	}
}

func TestLoadCustomValues(t *testing.T) {
	cfg, err := load(env(map[string]string{
		EnvFritzBoxURL:      "https://192.168.178.1:49443/",
		EnvFritzBoxUsername: " prometheus ",
		EnvFritzBoxPassword: " pass with spaces ",
		EnvCollectors:       "WAN, docsis",
		EnvListenAddress:    "127.0.0.1:9999",
		EnvLogLevel:         "debug",
		EnvScrapeTimeout:    "3s",
	}))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.FritzBoxURL != "https://192.168.178.1:49443" {
		t.Errorf("FritzBoxURL = %q (trailing slash should be removed)", cfg.FritzBoxURL)
	}
	if cfg.Username != "prometheus" {
		t.Errorf("Username = %q (should be trimmed)", cfg.Username)
	}
	if cfg.Password != " pass with spaces " {
		t.Errorf("Password = %q (must not be trimmed)", cfg.Password)
	}
	if want := []string{"wan", "docsis"}; !slices.Equal(cfg.Collectors, want) {
		t.Errorf("Collectors = %v, want %v", cfg.Collectors, want)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want DEBUG", cfg.LogLevel)
	}
	if cfg.ScrapeTimeout != 3*time.Second {
		t.Errorf("ScrapeTimeout = %v, want 3s", cfg.ScrapeTimeout)
	}
	if !cfg.CollectorEnabled("docsis") || cfg.CollectorEnabled("wlan") {
		t.Errorf("CollectorEnabled() does not match %v", cfg.Collectors)
	}
}

func TestLoadErrors(t *testing.T) {
	valid := map[string]string{
		EnvFritzBoxUsername: "prometheus",
		EnvFritzBoxPassword: "secret",
	}

	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{"missing username", EnvFritzBoxUsername, "", "FRITZBOX_USERNAME is required"},
		{"missing password", EnvFritzBoxPassword, "", "FRITZBOX_PASSWORD is required"},
		{"bad url scheme", EnvFritzBoxURL, "ftp://fritz.box", "scheme must be http or https"},
		{"url without host", EnvFritzBoxURL, "http://", "missing host"},
		{"bad log level", EnvLogLevel, "loud", "LOG_LEVEL"},
		{"bad timeout", EnvScrapeTimeout, "ten", "SCRAPE_TIMEOUT"},
		{"zero timeout", EnvScrapeTimeout, "0s", "must be positive"},
		{"unknown collector", EnvCollectors, "wan,dect", `unknown collector "dect"`},
		{"duplicate collector", EnvCollectors, "wan,wan", "listed twice"},
		{"no collector", EnvCollectors, " , ", "at least one collector"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{}
			for k, v := range valid {
				vars[k] = v
			}
			vars[tt.key] = tt.value

			_, err := load(env(vars))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := load(env(map[string]string{EnvLogLevel: "loud"}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{EnvFritzBoxUsername, EnvFritzBoxPassword, EnvLogLevel} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLogValueRedactsPassword(t *testing.T) {
	cfg := Config{Username: "prometheus", Password: "super-secret"}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config", "config", cfg)

	if strings.Contains(buf.String(), "super-secret") {
		t.Fatalf("password leaked into log output: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "[REDACTED]") {
		t.Errorf("expected redaction marker in: %s", buf.String())
	}
}
