package collector

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Options configure New.
type Options struct {
	// Collectors are the names of the enabled Collectors, in the order
	// they run. A nil list enables every known Collector in the built-in
	// order.
	Collectors []string
	// TR064 is the client for the TR-064 API.
	TR064 Caller
	// WebUI is the client for the web UI.
	WebUI PageFetcher
	// ScrapeTimeout limits one complete scrape.
	ScrapeTimeout time.Duration
	Logger        *slog.Logger
}

// client is a box client a Collector needs.
type client struct {
	name    string // used in error messages
	present func(o Options) bool
}

var (
	tr064Client = client{name: "TR-064 client", present: func(o Options) bool { return o.TR064 != nil }}
	webUIClient = client{name: "web UI client", present: func(o Options) bool { return o.WebUI != nil }}
)

// knownCollector is one entry of the name table.
type knownCollector struct {
	name  string
	needs client
	build func(o Options) Scraper
}

// knownCollectors is the only place a Collector name is written. It is
// ordered so that the default order and the names listed in error messages
// stay stable.
var knownCollectors = []knownCollector{
	{name: "device", needs: tr064Client, build: func(o Options) Scraper { return newDevice(o.TR064) }},
	{name: "wan", needs: tr064Client, build: func(o Options) Scraper { return newWAN(o.TR064) }},
	{name: "wlan", needs: tr064Client, build: func(o Options) Scraper { return newWLAN(o.TR064, wlanInterfaces) }},
	// DOCSIS data is only available via the web UI, not via TR-064.
	{name: "docsis", needs: webUIClient, build: func(o Options) Scraper { return newDOCSIS(o.WebUI) }},
}

// New builds an Exporter that runs the Collectors named in o.Collectors,
// or every known Collector if o.Collectors is nil. It checks every name and the client each Collector needs, and returns one
// error describing all problems. A client no enabled Collector needs may be
// nil.
func New(o Options) (*Exporter, error) {
	var (
		collectors []namedScraper
		errs       []error
	)
	names := o.Collectors
	if names == nil {
		names = knownNames()
	}
	for _, name := range names {
		known, ok := lookupCollector(name)
		if !ok {
			errs = append(errs, fmt.Errorf("unknown collector %q (known: %s)", name, strings.Join(knownNames(), ", ")))
			continue
		}
		if !known.needs.present(o) {
			errs = append(errs, fmt.Errorf("collector %q needs a %s, but none was given", name, known.needs.name))
			continue
		}
		collectors = append(collectors, namedScraper{name: name, scraper: known.build(o)})
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid collectors: %w", errors.Join(errs...))
	}

	enabled := make([]string, len(collectors))
	for i, c := range collectors {
		enabled[i] = c.name
	}
	o.Logger.Info("collectors enabled", "collectors", enabled)

	return newExporter(collectors, o.ScrapeTimeout, o.Logger), nil
}

func lookupCollector(name string) (knownCollector, bool) {
	for _, k := range knownCollectors {
		if k.name == name {
			return k, true
		}
	}
	return knownCollector{}, false
}

func knownNames() []string {
	names := make([]string, len(knownCollectors))
	for i, k := range knownCollectors {
		names[i] = k.name
	}
	return names
}
