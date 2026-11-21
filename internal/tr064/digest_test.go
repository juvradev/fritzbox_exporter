package tr064

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Test vector from RFC 2617, section 3.5.
func TestDigestResponseRFC2617(t *testing.T) {
	got := digestResponse(
		"Mufasa", "Circle Of Life", "testrealm@host.com",
		"GET", "/dir/index.html",
		"dcd98b7102dd2f0e8b11d0f600bfb0c093", "00000001", "0a4f113b", "auth",
	)
	want := "6629fae49393a05397450978507c4ef1"
	if got != want {
		t.Errorf("digestResponse() = %s, want %s", got, want)
	}
}

func TestParseChallenge(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		want    challenge
		wantErr bool
	}{
		{
			name:   "fritzbox",
			header: `Digest realm="F!Box SOAP-Auth", nonce="5C3D2A1B", algorithm=MD5, qop="auth"`,
			want:   challenge{realm: "F!Box SOAP-Auth", nonce: "5C3D2A1B", algorithm: "MD5", qop: "auth"},
		},
		{
			name:   "comma inside quotes",
			header: `Digest realm="a, b", nonce="n", qop="auth,auth-int"`,
			want:   challenge{realm: "a, b", nonce: "n", qop: "auth,auth-int"},
		},
		{name: "basic auth", header: `Basic realm="x"`, wantErr: true},
		{name: "missing nonce", header: `Digest realm="x"`, wantErr: true},
		{name: "unsupported algorithm", header: `Digest nonce="n", algorithm=SHA-256`, wantErr: true},
		{name: "unsupported qop", header: `Digest nonce="n", qop="auth-int"`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChallenge(tt.header)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseChallenge() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseChallenge() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDigestTransportAuthenticates(t *testing.T) {
	const (
		user  = "prometheus"
		pass  = "secret"
		realm = "F!Box SOAP-Auth"
		nonce = "ABCDEF"
		body  = "<soap/>"
	)

	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++

		gotBody, _ := io.ReadAll(r.Body)
		if string(gotBody) != body {
			t.Errorf("request %d: body = %q, want %q", requests, gotBody, body)
		}

		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Set("WWW-Authenticate", `Digest realm="`+realm+`", nonce="`+nonce+`", algorithm=MD5, qop="auth"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Verify the response hash the same way the box would.
		p := parseParams(strings.TrimPrefix(auth, "Digest "))
		want := digestResponse(user, pass, realm, r.Method, r.URL.RequestURI(), nonce, p["nc"], p["cnonce"], p["qop"])
		if p["response"] != want {
			t.Errorf("response = %s, want %s", p["response"], want)
		}
		if p["username"] != user || p["uri"] != r.URL.RequestURI() {
			t.Errorf("unexpected Authorization header: %s", auth)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &DigestTransport{
		Username:  user,
		Password:  pass,
		newCnonce: func() string { return "fixedcnonce" },
	}}

	resp, err := client.Post(srv.URL+"/upnp/control/deviceinfo", "text/xml", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if requests != 2 {
		t.Errorf("server saw %d requests, want 2", requests)
	}
}

func TestDigestTransportPassesThroughWithoutChallenge(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "" {
			t.Error("Authorization header sent without a challenge")
		}
	}))
	defer srv.Close()

	client := &http.Client{Transport: &DigestTransport{Username: "u", Password: "p"}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if requests != 1 {
		t.Errorf("server saw %d requests, want 1", requests)
	}
}

// digestServer is a fake TR-064 endpoint that checks digest credentials.
// With singleUse it accepts every nonce only once, like some servers do.
type digestServer struct {
	t         *testing.T
	singleUse bool

	mu       sync.Mutex
	requests int
	rejected int               // requests with credentials that were rejected
	issued   int               // number of nonces handed out
	lastNC   map[string]uint64 // highest nonce count seen per nonce
}

func newDigestServer(t *testing.T, singleUse bool) (*digestServer, *httptest.Server) {
	t.Helper()
	d := &digestServer{t: t, singleUse: singleUse, lastNC: map[string]uint64{}}
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	return d, srv
}

func (d *digestServer) challenge(w http.ResponseWriter) {
	d.issued++
	nonce := fmt.Sprintf("nonce-%d", d.issued)
	d.lastNC[nonce] = 0
	w.Header().Set("WWW-Authenticate", `Digest realm="F!Box SOAP-Auth", nonce="`+nonce+`", algorithm=MD5, qop="auth"`)
	w.WriteHeader(http.StatusUnauthorized)
}

func (d *digestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests++

	auth := r.Header.Get("Authorization")
	if auth == "" {
		d.challenge(w)
		return
	}

	p := parseParams(strings.TrimPrefix(auth, "Digest "))
	last, known := d.lastNC[p["nonce"]]
	nc, _ := strconv.ParseUint(p["nc"], 16, 32)
	want := digestResponse("prometheus", "secret", "F!Box SOAP-Auth", r.Method, r.URL.RequestURI(),
		p["nonce"], p["nc"], p["cnonce"], p["qop"])

	switch {
	case !known || nc <= last || p["response"] != want:
		d.rejected++
		d.challenge(w) // unknown or replayed nonce, or wrong credentials
	case d.singleUse && last > 0:
		d.rejected++
		d.challenge(w) // nonce was already used once
	default:
		d.lastNC[p["nonce"]] = nc
		w.WriteHeader(http.StatusOK)
	}
}

func digestClient() *http.Client {
	return &http.Client{Transport: &DigestTransport{Username: "prometheus", Password: "secret"}}
}

func doRequests(t *testing.T, c *http.Client, url string, n int) {
	t.Helper()
	for i := range n {
		resp, err := c.Post(url, "text/xml", strings.NewReader("<soap/>"))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200", i, resp.StatusCode)
		}
	}
}

func TestDigestTransportReusesNonce(t *testing.T) {
	d, srv := newDigestServer(t, false)

	doRequests(t, digestClient(), srv.URL+"/upnp/control/deviceinfo", 3)

	// 1 challenge + 3 authenticated requests instead of 3 × 2.
	if d.requests != 4 {
		t.Errorf("server saw %d requests, want 4", d.requests)
	}
	if d.issued != 1 {
		t.Errorf("server issued %d nonces, want 1", d.issued)
	}
}

func TestDigestTransportFallsBackForSingleUseNonces(t *testing.T) {
	d, srv := newDigestServer(t, true)

	doRequests(t, digestClient(), srv.URL+"/upnp/control/deviceinfo", 6)

	// Every call still succeeds with 2 requests: a rejected reuse attempt
	// gets a fresh challenge in its 401 response, which is answered directly.
	if d.requests != 12 {
		t.Errorf("server saw %d requests, want 12", d.requests)
	}
	// But each rejected reuse looks like a failed login to the server, so
	// reuse must stop after maxReuseRejections attempts.
	if d.rejected != maxReuseRejections {
		t.Errorf("server rejected %d logins, want %d", d.rejected, maxReuseRejections)
	}
}

func TestDigestAuthorizationEscapesQuotes(t *testing.T) {
	tr := &DigestTransport{Username: `pro"me\theus`, Password: "p", newCnonce: func() string { return "c" }}
	got := tr.authorization(challenge{realm: "r", nonce: "n", qop: "auth"}, 1, "POST", "/x")

	if !strings.Contains(got, `username="pro\"me\\theus"`) {
		t.Errorf("username not escaped: %s", got)
	}
}
