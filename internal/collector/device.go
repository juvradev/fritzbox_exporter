package collector

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/juvradev/fritzbox_exporter/internal/tr064"
)

// Device collects general information about the box (DeviceInfo service).
type Device struct {
	caller Caller
	info   *prometheus.Desc
	uptime *prometheus.Desc
}

// newDevice creates the Collector for general box information.
func newDevice(caller Caller) *Device {
	return &Device{
		caller: caller,
		info: prometheus.NewDesc(
			namespace+"_info",
			"Model and firmware of the FRITZ!Box. Always 1.",
			[]string{"model", "firmware"}, nil,
		),
		uptime: prometheus.NewDesc(
			namespace+"_uptime_seconds",
			"Time since the last reboot of the FRITZ!Box.",
			nil, nil,
		),
	}
}

// Describe implements Scraper.
func (d *Device) Describe(ch chan<- *prometheus.Desc) {
	ch <- d.info
	ch <- d.uptime
}

// Scrape implements Scraper.
func (d *Device) Scrape(ctx context.Context, ch chan<- prometheus.Metric) error {
	values, err := d.caller.Call(ctx, tr064.DeviceInfo, "GetInfo", nil)
	if err != nil {
		return err
	}

	// Note: GetInfo also returns the serial number and NewDeviceLog (which
	// contains the public IP). Both are deliberately not exported.
	ch <- prometheus.MustNewConstMetric(d.info, prometheus.GaugeValue, 1,
		values["NewModelName"], values["NewSoftwareVersion"])

	uptime, err := parseFloat(values, "NewUpTime")
	if err != nil {
		return err
	}
	ch <- prometheus.MustNewConstMetric(d.uptime, prometheus.GaugeValue, uptime)
	return nil
}
