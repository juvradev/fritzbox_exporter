package collector

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeFetcher returns a fixed web UI response.
type fakeFetcher struct {
	body []byte
	err  error
}

func (f fakeFetcher) Data(_ context.Context, page string) ([]byte, error) {
	if page != "docInfo" {
		return nil, errors.New("unexpected page " + page)
	}
	return f.body, f.err
}

func docsisExporter(t *testing.T, fixture string) *Exporter {
	t.Helper()
	body, err := os.ReadFile("testdata/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	return newExporter([]namedScraper{{"docsis", newDOCSIS(fakeFetcher{body: body})}}, 5*time.Second, slog.New(slog.DiscardHandler))
}

func TestDOCSISUnavailable(t *testing.T) {
	e := docsisExporter(t, "docinfo_unavailable.json")

	// Cable modem not in use: available=0, no channel metrics, and the
	// collector still counts as successful.
	want := `
# HELP fritzbox_docsis_available Whether DOCSIS channel data is available (1) or the cable modem is not in use (0).
# TYPE fritzbox_docsis_available gauge
fritzbox_docsis_available 0
# HELP fritzbox_scrape_collector_success Whether the collector succeeded during the last scrape.
# TYPE fritzbox_scrape_collector_success gauge
fritzbox_scrape_collector_success{collector="docsis"} 1
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want),
		"fritzbox_docsis_available", "fritzbox_scrape_collector_success"); err != nil {
		t.Error(err)
	}
	if n := testutil.CollectAndCount(e, "fritzbox_docsis_power_level_dbuv"); n != 0 {
		t.Errorf("got %d power metrics, want 0", n)
	}
}

func TestDOCSISChannels(t *testing.T) {
	e := docsisExporter(t, "docinfo_cable.json")

	want := `
# HELP fritzbox_docsis_available Whether DOCSIS channel data is available (1) or the cable modem is not in use (0).
# TYPE fritzbox_docsis_available gauge
fritzbox_docsis_available 1
# HELP fritzbox_docsis_power_level_dbuv Power level of a DOCSIS channel in dBµV as shown in the FRITZ!Box web UI.
# TYPE fritzbox_docsis_power_level_dbuv gauge
fritzbox_docsis_power_level_dbuv{channel="1",direction="downstream",docsis="3.0"} 5.6
fritzbox_docsis_power_level_dbuv{channel="2",direction="downstream",docsis="3.0"} 4.9
fritzbox_docsis_power_level_dbuv{channel="33",direction="downstream",docsis="3.1"} 7.1
fritzbox_docsis_power_level_dbuv{channel="1",direction="upstream",docsis="3.0"} 43
# HELP fritzbox_docsis_downstream_mer_db Modulation error ratio (signal-to-noise) of a downstream channel. Higher is better.
# TYPE fritzbox_docsis_downstream_mer_db gauge
fritzbox_docsis_downstream_mer_db{channel="1",docsis="3.0"} 36.4
fritzbox_docsis_downstream_mer_db{channel="2",docsis="3.0"} 35.8
fritzbox_docsis_downstream_mer_db{channel="33",docsis="3.1"} 41
# HELP fritzbox_docsis_downstream_corrected_errors_total Codewords with errors that the modem could correct.
# TYPE fritzbox_docsis_downstream_corrected_errors_total counter
fritzbox_docsis_downstream_corrected_errors_total{channel="1",docsis="3.0"} 123
fritzbox_docsis_downstream_corrected_errors_total{channel="2",docsis="3.0"} 456
# HELP fritzbox_docsis_downstream_uncorrectable_errors_total Codewords with errors that could not be corrected (lost data).
# TYPE fritzbox_docsis_downstream_uncorrectable_errors_total counter
fritzbox_docsis_downstream_uncorrectable_errors_total{channel="1",docsis="3.0"} 0
fritzbox_docsis_downstream_uncorrectable_errors_total{channel="2",docsis="3.0"} 2
# HELP fritzbox_docsis_channel_info Frequency and modulation of a DOCSIS channel. Always 1.
# TYPE fritzbox_docsis_channel_info gauge
fritzbox_docsis_channel_info{channel="1",direction="downstream",docsis="3.0",frequency="602",modulation="256QAM"} 1
fritzbox_docsis_channel_info{channel="2",direction="downstream",docsis="3.0",frequency="610",modulation="256QAM"} 1
fritzbox_docsis_channel_info{channel="33",direction="downstream",docsis="3.1",frequency="751 - 861",modulation="4096QAM"} 1
fritzbox_docsis_channel_info{channel="1",direction="upstream",docsis="3.0",frequency="51",modulation="64QAM"} 1
fritzbox_docsis_channel_info{channel="9",direction="upstream",docsis="3.1",frequency="29.8 - 64.8",modulation="1024QAM"} 1
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want),
		"fritzbox_docsis_available",
		"fritzbox_docsis_power_level_dbuv",
		"fritzbox_docsis_downstream_mer_db",
		"fritzbox_docsis_downstream_corrected_errors_total",
		"fritzbox_docsis_downstream_uncorrectable_errors_total",
		"fritzbox_docsis_channel_info",
	); err != nil {
		t.Error(err)
	}
}

func TestDOCSISSkipsDuplicateChannels(t *testing.T) {
	body := []byte(`{"pid":"docInfo","data":{"channelDs":{"docsis30":[
		{"channelID":1,"powerLevel":"5"},{"channelID":1,"powerLevel":"6"}]}}}`)
	e := newExporter([]namedScraper{{"docsis", newDOCSIS(fakeFetcher{body: body})}}, time.Second, slog.New(slog.DiscardHandler))

	// A duplicate label set would make the registry reject the whole scrape.
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(e)
	if _, err := reg.Gather(); err != nil {
		t.Fatal(err)
	}
}

func TestDOCSISFetchError(t *testing.T) {
	e := newExporter([]namedScraper{{"docsis", newDOCSIS(fakeFetcher{err: errors.New("login failed")})}},
		time.Second, slog.New(slog.DiscardHandler))

	want := `
# HELP fritzbox_scrape_collector_success Whether the collector succeeded during the last scrape.
# TYPE fritzbox_scrape_collector_success gauge
fritzbox_scrape_collector_success{collector="docsis"} 0
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want), "fritzbox_scrape_collector_success"); err != nil {
		t.Error(err)
	}
}

func TestFlexFloat(t *testing.T) {
	tests := []struct {
		in        string
		wantValue float64
		wantValid bool
	}{
		{`36.4`, 36.4, true},
		{`"36.4"`, 36.4, true},
		{`"-36.4"`, -36.4, true},
		{`" 7 "`, 7, true},
		{`"-"`, 0, false},
		{`""`, 0, false},
		{`null`, 0, false},
	}
	for _, tt := range tests {
		var f flexFloat
		if err := f.UnmarshalJSON([]byte(tt.in)); err != nil {
			t.Fatalf("%s: %v", tt.in, err)
		}
		if f.Value != tt.wantValue || f.Valid != tt.wantValid {
			t.Errorf("%s: got {%v %v}, want {%v %v}", tt.in, f.Value, f.Valid, tt.wantValue, tt.wantValid)
		}
	}
}
