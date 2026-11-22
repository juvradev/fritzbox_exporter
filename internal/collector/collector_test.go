package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/juvradev/fritzbox_exporter/internal/tr064"
)

// fakeCaller answers TR-064 calls from a map instead of a real box.
type fakeCaller struct {
	responses map[string]map[string]string // key: "<service type>#<action>"
	errs      map[string]error
	errAll    error         // if set, every call fails with it
	calls     *atomic.Int32 // optional call counter
}

func key(svc tr064.Service, action string) string { return svc.Type + "#" + action }

func (f fakeCaller) Call(_ context.Context, svc tr064.Service, action string, _ map[string]string) (map[string]string, error) {
	if f.calls != nil {
		f.calls.Add(1)
	}
	if f.errAll != nil {
		return nil, f.errAll
	}
	k := key(svc, action)
	if err := f.errs[k]; err != nil {
		return nil, err
	}
	v, ok := f.responses[k]
	if !ok {
		return nil, fmt.Errorf("unexpected call %s", k)
	}
	return v, nil
}

// spikeResponses are the (sanitized) values recorded during the spike.
func spikeResponses() map[string]map[string]string {
	return map[string]map[string]string{
		key(tr064.DeviceInfo, "GetInfo"): {
			"NewModelName":       "FRITZ!Box 6660 Cable",
			"NewSoftwareVersion": "252.08.25",
			"NewUpTime":          "2518554",
		},
		key(tr064.WANCommonInterfaceConfig, "GetCommonLinkProperties"): {
			"NewWANAccessType":              "Ethernet",
			"NewLayer1UpstreamMaxBitRate":   "2500000",
			"NewLayer1DownstreamMaxBitRate": "2500000",
			"NewPhysicalLinkStatus":         "Up",
		},
		key(tr064.WANCommonInterfaceConfig, "GetTotalBytesReceived"): {"NewTotalBytesReceived": "1441122562831"},
		key(tr064.WANCommonInterfaceConfig, "GetTotalBytesSent"):     {"NewTotalBytesSent": "114348569732"},
		key(tr064.WANIPConnection, "GetStatusInfo"): {
			"NewConnectionStatus":    "Connected",
			"NewLastConnectionError": "ERROR_NONE",
			"NewUptime":              "2518602",
		},
		key(tr064.WLANConfiguration(1), "GetInfo"): {
			"NewEnable": "1", "NewSSID": "home", "NewStandard": "ax", "NewChannel": "1",
			"NewX_AVM-DE_FrequencyBand": "2400",
		},
		key(tr064.WLANConfiguration(1), "GetTotalAssociations"): {"NewTotalAssociations": "1"},
		key(tr064.WLANConfiguration(2), "GetInfo"): {
			"NewEnable": "1", "NewSSID": "home", "NewStandard": "ax", "NewChannel": "116",
			"NewX_AVM-DE_FrequencyBand": "5000",
		},
		key(tr064.WLANConfiguration(2), "GetTotalAssociations"): {"NewTotalAssociations": "7"},
		key(tr064.WLANConfiguration(3), "GetInfo"): {
			"NewEnable": "0", "NewSSID": "guest", "NewStandard": "", "NewChannel": "1",
			"NewX_AVM-DE_FrequencyBand": "unknown",
		},
		key(tr064.WLANConfiguration(3), "GetTotalAssociations"): {"NewTotalAssociations": "0"},
	}
}

func tr064Exporter(caller Caller) *Exporter {
	return newExporter([]namedScraper{
		{"device", newDevice(caller)},
		{"wan", newWAN(caller)},
		{"wlan", newWLAN(caller, wlanInterfaces)},
	}, 5*time.Second, slog.New(slog.DiscardHandler))
}

func TestDeviceMetrics(t *testing.T) {
	e := tr064Exporter(fakeCaller{responses: spikeResponses()})

	want := `
# HELP fritzbox_info Model and firmware of the FRITZ!Box. Always 1.
# TYPE fritzbox_info gauge
fritzbox_info{firmware="252.08.25",model="FRITZ!Box 6660 Cable"} 1
# HELP fritzbox_uptime_seconds Time since the last reboot of the FRITZ!Box.
# TYPE fritzbox_uptime_seconds gauge
fritzbox_uptime_seconds 2518554
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want),
		"fritzbox_info", "fritzbox_uptime_seconds"); err != nil {
		t.Error(err)
	}
}

func TestWANMetrics(t *testing.T) {
	e := tr064Exporter(fakeCaller{responses: spikeResponses()})

	want := `
# HELP fritzbox_wan_info WAN access type, e.g. Ethernet or X_AVM-DE_Cable. Always 1.
# TYPE fritzbox_wan_info gauge
fritzbox_wan_info{access_type="Ethernet"} 1
# HELP fritzbox_wan_bytes_received_total Total bytes received on the WAN interface since the last reboot.
# TYPE fritzbox_wan_bytes_received_total counter
fritzbox_wan_bytes_received_total 1441122562831
# HELP fritzbox_wan_bytes_sent_total Total bytes sent on the WAN interface since the last reboot.
# TYPE fritzbox_wan_bytes_sent_total counter
fritzbox_wan_bytes_sent_total 114348569732
# HELP fritzbox_wan_max_bitrate_bits_per_second Maximum bit rate of the physical WAN link as reported by the box.
# TYPE fritzbox_wan_max_bitrate_bits_per_second gauge
fritzbox_wan_max_bitrate_bits_per_second{direction="downstream"} 2500000
fritzbox_wan_max_bitrate_bits_per_second{direction="upstream"} 2500000
# HELP fritzbox_wan_physical_link_up Whether the physical WAN link is up (1) or not (0).
# TYPE fritzbox_wan_physical_link_up gauge
fritzbox_wan_physical_link_up 1
# HELP fritzbox_wan_connected Whether the internet connection is established (1) or not (0).
# TYPE fritzbox_wan_connected gauge
fritzbox_wan_connected 1
# HELP fritzbox_wan_connection_uptime_seconds Time since the internet connection was established. A drop to 0 means a reconnect.
# TYPE fritzbox_wan_connection_uptime_seconds gauge
fritzbox_wan_connection_uptime_seconds 2518602
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want),
		"fritzbox_wan_info",
		"fritzbox_wan_bytes_received_total",
		"fritzbox_wan_bytes_sent_total",
		"fritzbox_wan_max_bitrate_bits_per_second",
		"fritzbox_wan_physical_link_up",
		"fritzbox_wan_connected",
		"fritzbox_wan_connection_uptime_seconds",
	); err != nil {
		t.Error(err)
	}
}

func TestWLANMetrics(t *testing.T) {
	e := tr064Exporter(fakeCaller{responses: spikeResponses()})

	want := `
# HELP fritzbox_wlan_associations Number of clients connected to the WLAN interface.
# TYPE fritzbox_wlan_associations gauge
fritzbox_wlan_associations{band="2.4GHz",index="1"} 1
fritzbox_wlan_associations{band="5GHz",index="2"} 7
fritzbox_wlan_associations{band="unknown",index="3"} 0
# HELP fritzbox_wlan_enabled Whether the WLAN interface is enabled (1) or not (0).
# TYPE fritzbox_wlan_enabled gauge
fritzbox_wlan_enabled{band="2.4GHz",index="1"} 1
fritzbox_wlan_enabled{band="5GHz",index="2"} 1
fritzbox_wlan_enabled{band="unknown",index="3"} 0
# HELP fritzbox_wlan_info SSID, standard and channel of a WLAN interface. Always 1.
# TYPE fritzbox_wlan_info gauge
fritzbox_wlan_info{band="2.4GHz",channel="1",index="1",ssid="home",standard="ax"} 1
fritzbox_wlan_info{band="5GHz",channel="116",index="2",ssid="home",standard="ax"} 1
fritzbox_wlan_info{band="unknown",channel="1",index="3",ssid="guest",standard=""} 1
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want),
		"fritzbox_wlan_associations", "fritzbox_wlan_enabled", "fritzbox_wlan_info"); err != nil {
		t.Error(err)
	}
}

func TestScrapeStatusAllSucceed(t *testing.T) {
	e := tr064Exporter(fakeCaller{responses: spikeResponses()})

	want := `
# HELP fritzbox_up Whether the FRITZ!Box could be reached (1 if at least one collector succeeded).
# TYPE fritzbox_up gauge
fritzbox_up 1
# HELP fritzbox_scrape_collector_success Whether the collector succeeded during the last scrape.
# TYPE fritzbox_scrape_collector_success gauge
fritzbox_scrape_collector_success{collector="device"} 1
fritzbox_scrape_collector_success{collector="wan"} 1
fritzbox_scrape_collector_success{collector="wlan"} 1
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want),
		"fritzbox_up", "fritzbox_scrape_collector_success"); err != nil {
		t.Error(err)
	}
}

func TestScrapeStatusOneFails(t *testing.T) {
	e := tr064Exporter(fakeCaller{
		responses: spikeResponses(),
		errs: map[string]error{
			key(tr064.WANIPConnection, "GetStatusInfo"): errors.New("boom"),
		},
	})

	want := `
# HELP fritzbox_up Whether the FRITZ!Box could be reached (1 if at least one collector succeeded).
# TYPE fritzbox_up gauge
fritzbox_up 1
# HELP fritzbox_scrape_collector_success Whether the collector succeeded during the last scrape.
# TYPE fritzbox_scrape_collector_success gauge
fritzbox_scrape_collector_success{collector="device"} 1
fritzbox_scrape_collector_success{collector="wan"} 0
fritzbox_scrape_collector_success{collector="wlan"} 1
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want),
		"fritzbox_up", "fritzbox_scrape_collector_success"); err != nil {
		t.Error(err)
	}

	// Metrics collected before the error are still exported.
	if n := testutil.CollectAndCount(e, "fritzbox_wan_bytes_received_total"); n != 1 {
		t.Errorf("fritzbox_wan_bytes_received_total count = %d, want 1", n)
	}
}

func TestScrapeStatusBoxDown(t *testing.T) {
	// No responses at all: every call fails, like an unreachable box.
	e := tr064Exporter(fakeCaller{})

	want := `
# HELP fritzbox_up Whether the FRITZ!Box could be reached (1 if at least one collector succeeded).
# TYPE fritzbox_up gauge
fritzbox_up 0
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want), "fritzbox_up"); err != nil {
		t.Error(err)
	}
}

// TestRegistryAcceptsExporter makes sure Describe and Collect agree, which
// the registry checks on every scrape (e.g. no duplicate or undeclared metrics).
func TestRegistryAcceptsExporter(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(tr064Exporter(fakeCaller{responses: spikeResponses()}))

	if _, err := reg.Gather(); err != nil {
		t.Fatal(err)
	}
}

func TestBandLabel(t *testing.T) {
	for raw, want := range map[string]string{
		"2400": "2.4GHz", "5000": "5GHz", "6000": "6GHz", "unknown": "unknown", "": "unknown",
	} {
		if got := bandLabel(raw); got != want {
			t.Errorf("bandLabel(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestWANPartialResults(t *testing.T) {
	// The first WAN call fails, the other parts must still be collected.
	e := tr064Exporter(fakeCaller{
		responses: spikeResponses(),
		errs: map[string]error{
			key(tr064.WANCommonInterfaceConfig, "GetCommonLinkProperties"): errors.New("boom"),
		},
	})

	for _, name := range []string{
		"fritzbox_wan_bytes_received_total",
		"fritzbox_wan_bytes_sent_total",
		"fritzbox_wan_connected",
		"fritzbox_wan_connection_uptime_seconds",
	} {
		if n := testutil.CollectAndCount(e, name); n != 1 {
			t.Errorf("%s count = %d, want 1", name, n)
		}
	}
	if n := testutil.CollectAndCount(e, "fritzbox_wan_info"); n != 0 {
		t.Errorf("fritzbox_wan_info count = %d, want 0 (its call failed)", n)
	}
}

func TestWLANPartialResults(t *testing.T) {
	// Interface 1 fails, interfaces 2 and 3 must still be collected.
	e := tr064Exporter(fakeCaller{
		responses: spikeResponses(),
		errs: map[string]error{
			key(tr064.WLANConfiguration(1), "GetInfo"): errors.New("boom"),
		},
	})

	want := `
# HELP fritzbox_wlan_associations Number of clients connected to the WLAN interface.
# TYPE fritzbox_wlan_associations gauge
fritzbox_wlan_associations{band="5GHz",index="2"} 7
fritzbox_wlan_associations{band="unknown",index="3"} 0
# HELP fritzbox_scrape_collector_success Whether the collector succeeded during the last scrape.
# TYPE fritzbox_scrape_collector_success gauge
fritzbox_scrape_collector_success{collector="device"} 1
fritzbox_scrape_collector_success{collector="wan"} 1
fritzbox_scrape_collector_success{collector="wlan"} 0
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want),
		"fritzbox_wlan_associations", "fritzbox_scrape_collector_success"); err != nil {
		t.Error(err)
	}
}

func TestAuthErrorPausesCollector(t *testing.T) {
	var calls atomic.Int32
	caller := fakeCaller{errAll: fmt.Errorf("tr064 GetInfo: %w", tr064.ErrUnauthorized), calls: &calls}
	e := newExporter([]namedScraper{{"device", newDevice(caller)}}, time.Second, slog.New(slog.DiscardHandler))

	now := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }

	testutil.CollectAndCount(e) // fails with ErrUnauthorized -> pause
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls after first scrape = %d, want 1", got)
	}

	now = now.Add(DefaultAuthBackoff - time.Second)
	testutil.CollectAndCount(e) // still paused: the box must not be asked
	if got := calls.Load(); got != 1 {
		t.Errorf("calls while paused = %d, want 1", got)
	}

	now = now.Add(2 * time.Second)
	testutil.CollectAndCount(e) // pause over: try again
	if got := calls.Load(); got != 2 {
		t.Errorf("calls after pause = %d, want 2", got)
	}
}

func TestNonAuthErrorDoesNotPause(t *testing.T) {
	var calls atomic.Int32
	caller := fakeCaller{errAll: errors.New("connection refused"), calls: &calls}
	e := newExporter([]namedScraper{{"device", newDevice(caller)}}, time.Second, slog.New(slog.DiscardHandler))

	testutil.CollectAndCount(e)
	testutil.CollectAndCount(e)
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2 (network errors are retried on every scrape)", got)
	}
}

func TestLogsOnlyStateChanges(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	caller := &switchableCaller{fail: true}
	e := newExporter([]namedScraper{{"device", newDevice(caller)}}, time.Second, logger)

	for range 3 {
		testutil.CollectAndCount(e)
	}
	if n := strings.Count(buf.String(), "collector failed"); n != 1 {
		t.Errorf("logged %d failures for 3 failing scrapes, want 1:\n%s", n, buf.String())
	}

	caller.fail = false
	testutil.CollectAndCount(e)
	testutil.CollectAndCount(e)
	if n := strings.Count(buf.String(), "collector recovered"); n != 1 {
		t.Errorf("logged %d recoveries, want 1:\n%s", n, buf.String())
	}
}

// switchableCaller fails or answers with the spike data. Only used from one
// goroutine at a time in TestLogsOnlyStateChanges.
type switchableCaller struct{ fail bool }

func (s *switchableCaller) Call(ctx context.Context, svc tr064.Service, action string, args map[string]string) (map[string]string, error) {
	if s.fail {
		return nil, errors.New("connection refused")
	}
	return fakeCaller{responses: spikeResponses()}.Call(ctx, svc, action, args)
}
