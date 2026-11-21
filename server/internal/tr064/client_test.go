package tr064

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeBox starts a test server that checks the SOAP headers and answers
// with the given status and fixture file from testdata/.
func fakeBox(t *testing.T, wantAction string, status int, fixture string) *httptest.Server {
	t.Helper()

	payload, err := os.ReadFile("testdata/" + fixture)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("SoapAction"); got != wantAction {
			t.Errorf("SoapAction = %q, want %q", got, wantAction)
		}
		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(status)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCallParsesResponse(t *testing.T) {
	srv := fakeBox(t, DeviceInfo.Type+"#GetInfo", http.StatusOK, "deviceinfo_getinfo.xml")
	c := NewClient(srv.URL, "u", "p", 5*time.Second)

	got, err := c.Call(context.Background(), DeviceInfo, "GetInfo", nil)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"NewModelName":        "FRITZ!Box 6660 Cable",
		"NewSoftwareVersion":  "252.08.25",
		"NewUpTime":           "2518554",
		"NewProvisioningCode": "",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestCallReturnsSOAPError(t *testing.T) {
	srv := fakeBox(t, DeviceInfo.Type+"#DoesNotExist", http.StatusInternalServerError, "fault_invalid_action.xml")
	c := NewClient(srv.URL, "u", "p", 5*time.Second)

	_, err := c.Call(context.Background(), DeviceInfo, "DoesNotExist", nil)

	var soapErr *SOAPError
	if !errors.As(err, &soapErr) {
		t.Fatalf("error = %v, want *SOAPError", err)
	}
	if soapErr.Code != 401 || soapErr.Description != "Invalid Action" {
		t.Errorf("SOAPError = %+v", soapErr)
	}
}

func TestCallUnauthorized(t *testing.T) {
	// The box keeps answering 401, as it does for a wrong password.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Digest realm="F!Box SOAP-Auth", nonce="N", algorithm=MD5, qop="auth"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "u", "wrong", 5*time.Second)
	_, err := c.Call(context.Background(), DeviceInfo, "GetInfo", nil)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}

func TestCallSendsEnvelope(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
			`<u:GetGenericHostEntryResponse xmlns:u="x"/></s:Body></s:Envelope>`))
	}))
	defer srv.Close()

	svc := Service{Type: "urn:test:service:Hosts:1", ControlURL: "/upnp/control/hosts"}
	c := NewClient(srv.URL, "u", "p", 5*time.Second)
	_, err := c.Call(context.Background(), svc, "GetGenericHostEntry", map[string]string{"NewIndex": "<1&2>"})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`<u:GetGenericHostEntry xmlns:u="urn:test:service:Hosts:1">`,
		`<NewIndex>&lt;1&amp;2&gt;</NewIndex>`, // arguments must be XML-escaped
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body misses %q:\n%s", want, gotBody)
		}
	}
}

func TestWLANConfiguration(t *testing.T) {
	got := WLANConfiguration(2)
	want := Service{
		Type:       "urn:dslforum-org:service:WLANConfiguration:2",
		ControlURL: "/upnp/control/wlanconfig2",
	}
	if got != want {
		t.Errorf("WLANConfiguration(2) = %+v, want %+v", got, want)
	}
}
