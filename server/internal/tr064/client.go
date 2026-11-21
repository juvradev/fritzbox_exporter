// Package tr064 is a minimal client for the AVM TR-064 SOAP API.
//
// It only knows how to call an action and return the response arguments
// as strings; turning them into metrics is the job of the collectors.
package tr064

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// maxResponseSize protects against unexpectedly large responses.
const maxResponseSize = 1 << 20 // 1 MiB

// ErrUnauthorized is returned when the box rejects the credentials.
var ErrUnauthorized = errors.New("unauthorized: check username, password and user rights")

// Service identifies a TR-064 service: its type and the URL to send actions to.
// Both values come from tr64desc.xml.
type Service struct {
	Type       string
	ControlURL string
}

// Services used by the exporter.
var (
	DeviceInfo = Service{
		Type:       "urn:dslforum-org:service:DeviceInfo:1",
		ControlURL: "/upnp/control/deviceinfo",
	}
	WANCommonInterfaceConfig = Service{
		Type:       "urn:dslforum-org:service:WANCommonInterfaceConfig:1",
		ControlURL: "/upnp/control/wancommonifconfig1",
	}
	WANIPConnection = Service{
		Type:       "urn:dslforum-org:service:WANIPConnection:1",
		ControlURL: "/upnp/control/wanipconnection1",
	}
)

// WLANConfiguration returns the service for the WLAN interface with the
// given 1-based index (1 = 2.4 GHz, 2 = 5 GHz, 3 = guest on a 6660).
func WLANConfiguration(index int) Service {
	return Service{
		Type:       fmt.Sprintf("urn:dslforum-org:service:WLANConfiguration:%d", index),
		ControlURL: fmt.Sprintf("/upnp/control/wlanconfig%d", index),
	}
}

// SOAPError is a fault returned by the box, e.g. for an unknown action.
// Use errors.As to inspect it.
type SOAPError struct {
	Code        int
	Description string
}

func (e *SOAPError) Error() string {
	return fmt.Sprintf("soap fault %d: %s", e.Code, e.Description)
}

// Client calls TR-064 actions on a FRITZ!Box.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient creates a client for baseURL (e.g. "http://192.168.178.1:49000").
// timeout limits each call including the digest auth round trip.
func NewClient(baseURL, username, password string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout:   timeout,
			Transport: &DigestTransport{Username: username, Password: password},
		},
	}
}

// Call invokes action on svc and returns the response arguments,
// e.g. {"NewUpTime": "2518554"}. args may be nil.
func (c *Client) Call(ctx context.Context, svc Service, action string, args map[string]string) (map[string]string, error) {
	values, err := c.call(ctx, svc, action, args)
	if err != nil {
		return nil, fmt.Errorf("tr064 %s: %w", action, err)
	}
	return values, nil
}

func (c *Client) call(ctx context.Context, svc Service, action string, args map[string]string) (map[string]string, error) {
	// strings.Reader lets http.NewRequest set GetBody, which DigestTransport
	// needs to resend the body after the 401 challenge.
	body := strings.NewReader(buildEnvelope(svc.Type, action, args))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+svc.ControlURL, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SoapAction", svc.Type+"#"+action)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return parseResponse(data, action+"Response")
	case http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case http.StatusInternalServerError:
		// SOAP faults are sent with status 500.
		return nil, parseFault(data)
	default:
		return nil, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
}

// buildEnvelope renders the SOAP request body. Arguments are sorted by name
// so the output is deterministic (easier to test and debug).
func buildEnvelope(serviceType, action string, args map[string]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">`)
	b.WriteString(`<s:Body>`)
	fmt.Fprintf(&b, `<u:%s xmlns:u="%s">`, action, serviceType)
	for _, name := range slices.Sorted(maps.Keys(args)) {
		fmt.Fprintf(&b, "<%s>", name)
		_ = xml.EscapeText(&b, []byte(args[name])) // strings.Builder never fails
		fmt.Fprintf(&b, "</%s>", name)
	}
	fmt.Fprintf(&b, `</u:%s>`, action)
	b.WriteString(`</s:Body></s:Envelope>`)
	return b.String()
}

// soapEnvelope matches both successful responses and faults.
// Tags without a namespace match any namespace (s:Body, u:...Response).
type soapEnvelope struct {
	Body struct {
		Fault    *soapFault   `xml:"Fault"`
		Response soapResponse `xml:",any"` // first child that is not a Fault
	} `xml:"Body"`
}

type soapResponse struct {
	XMLName xml.Name
	Args    []soapArg `xml:",any"`
}

type soapArg struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type soapFault struct {
	FaultString string `xml:"faultstring"`
	Detail      struct {
		UPnPError struct {
			Code        int    `xml:"errorCode"`
			Description string `xml:"errorDescription"`
		} `xml:"UPnPError"`
	} `xml:"detail"`
}

// parseResponse extracts the arguments of the <u:{Action}Response> element.
func parseResponse(data []byte, responseName string) (map[string]string, error) {
	var env soapEnvelope
	if err := xml.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if got := env.Body.Response.XMLName.Local; got != responseName {
		return nil, fmt.Errorf("unexpected response element %q, want %q", got, responseName)
	}

	values := make(map[string]string, len(env.Body.Response.Args))
	for _, arg := range env.Body.Response.Args {
		values[arg.XMLName.Local] = arg.Value
	}
	return values, nil
}

// parseFault turns a SOAP fault body into a *SOAPError.
func parseFault(data []byte) error {
	var env soapEnvelope
	if err := xml.Unmarshal(data, &env); err != nil || env.Body.Fault == nil {
		return errors.New("HTTP 500 without a valid SOAP fault")
	}
	f := env.Body.Fault
	desc := f.Detail.UPnPError.Description
	if desc == "" {
		desc = f.FaultString
	}
	return &SOAPError{Code: f.Detail.UPnPError.Code, Description: desc}
}
