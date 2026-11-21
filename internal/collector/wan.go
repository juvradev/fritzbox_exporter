package collector

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/juvradev/fritzbox_exporter/internal/tr064"
)

// WAN collects throughput and connection state of the internet link
// (WANCommonInterfaceConfig and WANIPConnection services).
type WAN struct {
	caller Caller

	info             *prometheus.Desc
	bytesReceived    *prometheus.Desc
	bytesSent        *prometheus.Desc
	maxBitrate       *prometheus.Desc
	physicalLinkUp   *prometheus.Desc
	connected        *prometheus.Desc
	connectionUptime *prometheus.Desc
}

// NewWAN creates the "wan" scraper.
func NewWAN(caller Caller) *WAN {
	return &WAN{
		caller: caller,
		info: prometheus.NewDesc(
			namespace+"_wan_info",
			"WAN access type, e.g. Ethernet or X_AVM-DE_Cable. Always 1.",
			[]string{"access_type"}, nil,
		),
		bytesReceived: prometheus.NewDesc(
			namespace+"_wan_bytes_received_total",
			"Total bytes received on the WAN interface since the last reboot.",
			nil, nil,
		),
		bytesSent: prometheus.NewDesc(
			namespace+"_wan_bytes_sent_total",
			"Total bytes sent on the WAN interface since the last reboot.",
			nil, nil,
		),
		maxBitrate: prometheus.NewDesc(
			namespace+"_wan_max_bitrate_bits_per_second",
			"Maximum bit rate of the physical WAN link as reported by the box.",
			[]string{"direction"}, nil,
		),
		physicalLinkUp: prometheus.NewDesc(
			namespace+"_wan_physical_link_up",
			"Whether the physical WAN link is up (1) or not (0).",
			nil, nil,
		),
		connected: prometheus.NewDesc(
			namespace+"_wan_connected",
			"Whether the internet connection is established (1) or not (0).",
			nil, nil,
		),
		connectionUptime: prometheus.NewDesc(
			namespace+"_wan_connection_uptime_seconds",
			"Time since the internet connection was established. A drop to 0 means a reconnect.",
			nil, nil,
		),
	}
}

// Name implements Scraper.
func (w *WAN) Name() string { return "wan" }

// Describe implements Scraper.
func (w *WAN) Describe(ch chan<- *prometheus.Desc) {
	ch <- w.info
	ch <- w.bytesReceived
	ch <- w.bytesSent
	ch <- w.maxBitrate
	ch <- w.physicalLinkUp
	ch <- w.connected
	ch <- w.connectionUptime
}

// Scrape implements Scraper.
func (w *WAN) Scrape(ctx context.Context, ch chan<- prometheus.Metric) error {
	if err := w.scrapeLink(ctx, ch); err != nil {
		return err
	}
	if err := w.scrapeTraffic(ctx, ch); err != nil {
		return err
	}
	return w.scrapeConnection(ctx, ch)
}

// scrapeLink reports the physical link: access type, state and max bit rate.
func (w *WAN) scrapeLink(ctx context.Context, ch chan<- prometheus.Metric) error {
	v, err := w.caller.Call(ctx, tr064.WANCommonInterfaceConfig, "GetCommonLinkProperties", nil)
	if err != nil {
		return err
	}

	ch <- prometheus.MustNewConstMetric(w.info, prometheus.GaugeValue, 1, v["NewWANAccessType"])
	ch <- prometheus.MustNewConstMetric(w.physicalLinkUp, prometheus.GaugeValue,
		boolToFloat(v["NewPhysicalLinkStatus"] == "Up"))

	down, err := parseFloat(v, "NewLayer1DownstreamMaxBitRate")
	if err != nil {
		return err
	}
	up, err := parseFloat(v, "NewLayer1UpstreamMaxBitRate")
	if err != nil {
		return err
	}
	ch <- prometheus.MustNewConstMetric(w.maxBitrate, prometheus.GaugeValue, down, "downstream")
	ch <- prometheus.MustNewConstMetric(w.maxBitrate, prometheus.GaugeValue, up, "upstream")
	return nil
}

// scrapeTraffic reports the byte counters. On this firmware they are 64 bit
// (verified in the spike), so they do not wrap at 4 GiB.
func (w *WAN) scrapeTraffic(ctx context.Context, ch chan<- prometheus.Metric) error {
	counters := []struct {
		action, field string
		desc          *prometheus.Desc
	}{
		{"GetTotalBytesReceived", "NewTotalBytesReceived", w.bytesReceived},
		{"GetTotalBytesSent", "NewTotalBytesSent", w.bytesSent},
	}

	for _, c := range counters {
		v, err := w.caller.Call(ctx, tr064.WANCommonInterfaceConfig, c.action, nil)
		if err != nil {
			return err
		}
		value, err := parseFloat(v, c.field)
		if err != nil {
			return err
		}
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.CounterValue, value)
	}
	return nil
}

// scrapeConnection reports whether the internet connection is up and for how
// long. Reconnects show up in PromQL as resets(fritzbox_wan_connection_uptime_seconds[1d]).
func (w *WAN) scrapeConnection(ctx context.Context, ch chan<- prometheus.Metric) error {
	v, err := w.caller.Call(ctx, tr064.WANIPConnection, "GetStatusInfo", nil)
	if err != nil {
		return err
	}

	ch <- prometheus.MustNewConstMetric(w.connected, prometheus.GaugeValue,
		boolToFloat(v["NewConnectionStatus"] == "Connected"))

	uptime, err := parseFloat(v, "NewUptime")
	if err != nil {
		return err
	}
	ch <- prometheus.MustNewConstMetric(w.connectionUptime, prometheus.GaugeValue, uptime)
	return nil
}
