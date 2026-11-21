//go:build integration

package tr064

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestIntegrationDeviceInfo talks to a real FRITZ!Box. Run it with:
//
//	FRITZBOX_URL=http://192.168.178.1:49000 FRITZBOX_USERNAME=prometheus \
//	FRITZBOX_PASSWORD=... go test -tags integration -v ./internal/tr064
func TestIntegrationDeviceInfo(t *testing.T) {
	url := os.Getenv("FRITZBOX_URL")
	user := os.Getenv("FRITZBOX_USERNAME")
	pass := os.Getenv("FRITZBOX_PASSWORD")
	if url == "" || user == "" || pass == "" {
		t.Skip("FRITZBOX_URL, FRITZBOX_USERNAME and FRITZBOX_PASSWORD must be set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := NewClient(url, user, pass, 10*time.Second)

	info, err := c.Call(ctx, DeviceInfo, "GetInfo", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model=%s firmware=%s uptime=%ss",
		info["NewModelName"], info["NewSoftwareVersion"], info["NewUpTime"])

	for i := 1; i <= 3; i++ {
		v, err := c.Call(ctx, WLANConfiguration(i), "GetTotalAssociations", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("wlan%d clients=%s", i, v["NewTotalAssociations"])
	}
}
