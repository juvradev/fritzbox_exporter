package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// PageFetcher is the part of webui.Client the DOCSIS collector needs.
type PageFetcher interface {
	Data(ctx context.Context, page string) ([]byte, error)
}

// DOCSIS collects per-channel signal values of the cable modem from the
// web UI page "docInfo". If the cable modem is not in use (e.g. the box is
// connected via LAN), the page is hidden and only fritzbox_docsis_available 0
// is reported; that is not treated as an error.
type DOCSIS struct {
	fetcher PageFetcher

	available       *prometheus.Desc
	channelInfo     *prometheus.Desc
	power           *prometheus.Desc
	mer             *prometheus.Desc
	correctedErrs   *prometheus.Desc
	uncorrectedErrs *prometheus.Desc
}

// NewDOCSIS creates the "docsis" scraper.
func NewDOCSIS(fetcher PageFetcher) *DOCSIS {
	channel := []string{"direction", "docsis", "channel"}
	downstream := []string{"docsis", "channel"}
	return &DOCSIS{
		fetcher: fetcher,
		available: prometheus.NewDesc(
			namespace+"_docsis_available",
			"Whether DOCSIS channel data is available (1) or the cable modem is not in use (0).",
			nil, nil,
		),
		channelInfo: prometheus.NewDesc(
			namespace+"_docsis_channel_info",
			"Frequency and modulation of a DOCSIS channel. Always 1.",
			append(channel, "frequency", "modulation"), nil,
		),
		power: prometheus.NewDesc(
			namespace+"_docsis_power_level_dbuv",
			"Power level of a DOCSIS channel in dBµV as shown in the FRITZ!Box web UI.",
			channel, nil,
		),
		mer: prometheus.NewDesc(
			namespace+"_docsis_downstream_mer_db",
			"Modulation error ratio (signal-to-noise) of a downstream channel. Higher is better.",
			downstream, nil,
		),
		correctedErrs: prometheus.NewDesc(
			namespace+"_docsis_downstream_corrected_errors_total",
			"Codewords with errors that the modem could correct.",
			downstream, nil,
		),
		uncorrectedErrs: prometheus.NewDesc(
			namespace+"_docsis_downstream_uncorrectable_errors_total",
			"Codewords with errors that could not be corrected (lost data).",
			downstream, nil,
		),
	}
}

// Name implements Scraper.
func (d *DOCSIS) Name() string { return "docsis" }

// Describe implements Scraper.
func (d *DOCSIS) Describe(ch chan<- *prometheus.Desc) {
	ch <- d.available
	ch <- d.channelInfo
	ch <- d.power
	ch <- d.mer
	ch <- d.correctedErrs
	ch <- d.uncorrectedErrs
}

// Scrape implements Scraper.
func (d *DOCSIS) Scrape(ctx context.Context, ch chan<- prometheus.Metric) error {
	raw, err := d.fetcher.Data(ctx, "docInfo")
	if err != nil {
		return err
	}

	var page docInfoPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return fmt.Errorf("decode docInfo: %w", err)
	}

	// When DOCSIS is disabled the box answers with another page (e.g.
	// "overview") and marks docInfo as hidden (verified in the spike).
	available := page.PID == "docInfo" && !page.Hide["docInfo"]
	ch <- prometheus.MustNewConstMetric(d.available, prometheus.GaugeValue, boolToFloat(available))
	if !available {
		return nil
	}

	seen := make(map[string]bool)
	for _, g := range []struct {
		direction, docsis string
		channels          []docsisChannel
	}{
		{"downstream", "3.0", page.Data.ChannelDs.Docsis30},
		{"downstream", "3.1", page.Data.ChannelDs.Docsis31},
		{"upstream", "3.0", page.Data.ChannelUs.Docsis30},
		{"upstream", "3.1", page.Data.ChannelUs.Docsis31},
	} {
		for _, c := range g.channels {
			id := string(c.ChannelID)
			// Duplicate label sets would make the whole /metrics request fail.
			key := g.direction + "/" + g.docsis + "/" + id
			if id == "" || seen[key] {
				continue
			}
			seen[key] = true
			d.emitChannel(ch, g.direction, g.docsis, id, c)
		}
	}
	return nil
}

func (d *DOCSIS) emitChannel(ch chan<- prometheus.Metric, direction, docsis, id string, c docsisChannel) {
	ch <- prometheus.MustNewConstMetric(d.channelInfo, prometheus.GaugeValue, 1,
		direction, docsis, id, string(c.Frequency), c.Modulation)

	if c.PowerLevel.Valid {
		ch <- prometheus.MustNewConstMetric(d.power, prometheus.GaugeValue, c.PowerLevel.Value, direction, docsis, id)
	}
	if direction != "downstream" {
		return
	}

	// DOCSIS 3.1 reports MER directly. DOCSIS 3.0 reports MSE as a negative
	// value (e.g. -36.4 dB); its magnitude is the MER.
	switch {
	case c.MER.Valid:
		ch <- prometheus.MustNewConstMetric(d.mer, prometheus.GaugeValue, c.MER.Value, docsis, id)
	case c.MSE.Valid:
		ch <- prometheus.MustNewConstMetric(d.mer, prometheus.GaugeValue, math.Abs(c.MSE.Value), docsis, id)
	}

	if c.CorrErrors.Valid {
		ch <- prometheus.MustNewConstMetric(d.correctedErrs, prometheus.CounterValue, c.CorrErrors.Value, docsis, id)
	}
	if c.NonCorrErrors.Valid {
		ch <- prometheus.MustNewConstMetric(d.uncorrectedErrs, prometheus.CounterValue, c.NonCorrErrors.Value, docsis, id)
	}
}

// docInfoPage is the part of the data.lua response we use.
//
// NOTE: the channel fields follow the structure known from other FRITZ!Box
// cable models. They could not be verified yet because DOCSIS is disabled on
// the box until the move; replace testdata/docinfo_cable.json with a real,
// sanitized response once the cable is connected.
type docInfoPage struct {
	PID  string          `json:"pid"`
	Hide map[string]bool `json:"hide"`
	Data struct {
		ChannelDs channelGroup `json:"channelDs"`
		ChannelUs channelGroup `json:"channelUs"`
	} `json:"data"`
}

type channelGroup struct {
	Docsis30 []docsisChannel `json:"docsis30"`
	Docsis31 []docsisChannel `json:"docsis31"`
}

type docsisChannel struct {
	ChannelID     flexString `json:"channelID"`
	Frequency     flexString `json:"frequency"`
	Modulation    string     `json:"type"`
	PowerLevel    flexFloat  `json:"powerLevel"`
	MSE           flexFloat  `json:"mse"`
	MER           flexFloat  `json:"mer"`
	CorrErrors    flexFloat  `json:"corrErrors"`
	NonCorrErrors flexFloat  `json:"nonCorrErrors"`
}

// flexFloat accepts a JSON number or a numeric string ("36.4" or 36.4).
// Missing, empty or non-numeric values leave Valid false instead of failing
// the whole scrape.
type flexFloat struct {
	Value float64
	Valid bool
}

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(string(b), `"`))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil // e.g. null, "" or "-": treat as missing
	}
	f.Value, f.Valid = v, true
	return nil
}

// flexString accepts a JSON string or number and keeps its text form.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		return nil
	}
	*f = flexString(strings.TrimSpace(strings.Trim(s, `"`)))
	return nil
}
