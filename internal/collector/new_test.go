package collector

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newOptions returns Options with both fake clients and the given
// Collector names.
func newOptions(names ...string) Options {
	return Options{
		Collectors:    names,
		TR064:         fakeCaller{responses: spikeResponses()},
		WebUI:         fakeFetcher{},
		ScrapeTimeout: 5 * time.Second,
		Logger:        slog.New(slog.DiscardHandler),
	}
}

func TestNewRejectsUnknownCollector(t *testing.T) {
	_, err := New(newOptions("wan", "foo"))
	if err == nil {
		t.Fatal("New accepted an unknown Collector name")
	}
	for _, want := range []string{`"foo"`, "known: device, wan, wlan, docsis"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestNewEnablesExactlyTheListedCollectors(t *testing.T) {
	e, err := New(newOptions("wlan", "device"))
	if err != nil {
		t.Fatal(err)
	}

	want := `
# HELP fritzbox_scrape_collector_success Whether the collector succeeded during the last scrape.
# TYPE fritzbox_scrape_collector_success gauge
fritzbox_scrape_collector_success{collector="device"} 1
fritzbox_scrape_collector_success{collector="wlan"} 1
`
	if err := testutil.CollectAndCompare(e, strings.NewReader(want), "fritzbox_scrape_collector_success"); err != nil {
		t.Error(err)
	}
}

func TestNewRejectsMissingClient(t *testing.T) {
	o := newOptions("device", "docsis")
	o.WebUI = nil

	_, err := New(o)
	if err == nil {
		t.Fatal("New accepted docsis without a web UI client")
	}
	for _, want := range []string{`"docsis"`, "web UI client"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestNewAcceptsMissingClientNoCollectorNeeds(t *testing.T) {
	o := newOptions("docsis")
	o.TR064 = nil
	if _, err := New(o); err != nil {
		t.Errorf("docsis without a TR-064 client: %v", err)
	}

	o = newOptions("device", "wan", "wlan")
	o.WebUI = nil
	if _, err := New(o); err != nil {
		t.Errorf("TR-064 Collectors without a web UI client: %v", err)
	}
}

func TestNewReportsAllProblemsInOneError(t *testing.T) {
	o := newOptions("foo", "device", "bar", "wan", "docsis")
	o.TR064 = nil
	o.WebUI = nil

	_, err := New(o)
	if err == nil {
		t.Fatal("New accepted an invalid Collector list")
	}
	for _, want := range []string{`"foo"`, `"bar"`, `"device"`, `"wan"`, `"docsis"`, "TR-064 client", "web UI client"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// countingFetcher counts web UI requests and answers with the given body.
type countingFetcher struct {
	body  []byte
	calls *atomic.Int32
}

func (f countingFetcher) Data(_ context.Context, _ string) ([]byte, error) {
	f.calls.Add(1)
	return f.body, nil
}

func TestNewDOCSISOnlyNeverCallsTR064(t *testing.T) {
	var calls atomic.Int32
	o := newOptions("docsis")
	o.TR064 = fakeCaller{responses: spikeResponses(), calls: &calls}

	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	testutil.CollectAndCount(e)

	if got := calls.Load(); got != 0 {
		t.Errorf("TR-064 calls = %d, want 0", got)
	}
}

func TestNewWithoutDOCSISNeverCallsWebUI(t *testing.T) {
	var calls atomic.Int32
	o := newOptions("device", "wan", "wlan")
	o.WebUI = countingFetcher{calls: &calls}

	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	testutil.CollectAndCount(e)

	if got := calls.Load(); got != 0 {
		t.Errorf("web UI calls = %d, want 0", got)
	}
}

func TestNewLogsEnabledCollectors(t *testing.T) {
	var buf bytes.Buffer
	o := newOptions("wan", "docsis")
	o.Logger = slog.New(slog.NewTextHandler(&buf, nil))

	if _, err := New(o); err != nil {
		t.Fatal(err)
	}

	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Errorf("logged %d lines, want 1:\n%s", n, buf.String())
	}
	if want := `collectors="[wan docsis]"`; !strings.Contains(buf.String(), want) {
		t.Errorf("log %q does not contain %q", buf.String(), want)
	}
}

func TestNewKeepsTheGivenOrder(t *testing.T) {
	var buf bytes.Buffer
	o := newOptions("docsis", "wlan", "device")
	o.Logger = slog.New(slog.NewTextHandler(&buf, nil))

	if _, err := New(o); err != nil {
		t.Fatal(err)
	}

	if want := `collectors="[docsis wlan device]"`; !strings.Contains(buf.String(), want) {
		t.Errorf("log %q does not contain %q", buf.String(), want)
	}
}
