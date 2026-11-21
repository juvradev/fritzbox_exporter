package collector

import (
	"context"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/juvradev/fritzbox_exporter/internal/tr064"
)

// DefaultWLANInterfaces is the number of WLANConfiguration services on a
// dual-band box like the 6660: 2.4 GHz, 5 GHz and guest.
const DefaultWLANInterfaces = 3

// WLAN collects state and client counts of every WLAN interface
// (WLANConfiguration:1..n services).
type WLAN struct {
	caller     Caller
	interfaces int

	info         *prometheus.Desc
	enabled      *prometheus.Desc
	associations *prometheus.Desc
}

// NewWLAN creates the "wlan" scraper for the given number of interfaces.
func NewWLAN(caller Caller, interfaces int) *WLAN {
	labels := []string{"index", "band"}
	return &WLAN{
		caller:     caller,
		interfaces: interfaces,
		info: prometheus.NewDesc(
			namespace+"_wlan_info",
			"SSID, standard and channel of a WLAN interface. Always 1.",
			append(labels, "ssid", "standard", "channel"), nil,
		),
		enabled: prometheus.NewDesc(
			namespace+"_wlan_enabled",
			"Whether the WLAN interface is enabled (1) or not (0).",
			labels, nil,
		),
		associations: prometheus.NewDesc(
			namespace+"_wlan_associations",
			"Number of clients connected to the WLAN interface.",
			labels, nil,
		),
	}
}

// Name implements Scraper.
func (w *WLAN) Name() string { return "wlan" }

// Describe implements Scraper.
func (w *WLAN) Describe(ch chan<- *prometheus.Desc) {
	ch <- w.info
	ch <- w.enabled
	ch <- w.associations
}

// Scrape implements Scraper.
func (w *WLAN) Scrape(ctx context.Context, ch chan<- prometheus.Metric) error {
	for i := 1; i <= w.interfaces; i++ {
		if err := w.scrapeInterface(ctx, ch, i); err != nil {
			return err
		}
	}
	return nil
}

func (w *WLAN) scrapeInterface(ctx context.Context, ch chan<- prometheus.Metric, index int) error {
	svc := tr064.WLANConfiguration(index)

	info, err := w.caller.Call(ctx, svc, "GetInfo", nil)
	if err != nil {
		return err
	}
	assoc, err := w.caller.Call(ctx, svc, "GetTotalAssociations", nil)
	if err != nil {
		return err
	}

	idx := strconv.Itoa(index)
	band := bandLabel(info["NewX_AVM-DE_FrequencyBand"])

	ch <- prometheus.MustNewConstMetric(w.info, prometheus.GaugeValue, 1,
		idx, band, info["NewSSID"], info["NewStandard"], info["NewChannel"])
	ch <- prometheus.MustNewConstMetric(w.enabled, prometheus.GaugeValue,
		boolToFloat(info["NewEnable"] == "1"), idx, band)

	clients, err := parseFloat(assoc, "NewTotalAssociations")
	if err != nil {
		return err
	}
	ch <- prometheus.MustNewConstMetric(w.associations, prometheus.GaugeValue, clients, idx, band)
	return nil
}

// bandLabel turns AVM's frequency band ("2400", "5000", "6000") into a
// readable label. A disabled guest network reports "unknown".
func bandLabel(raw string) string {
	switch raw {
	case "2400":
		return "2.4GHz"
	case "5000":
		return "5GHz"
	case "6000":
		return "6GHz"
	default:
		return "unknown"
	}
}
