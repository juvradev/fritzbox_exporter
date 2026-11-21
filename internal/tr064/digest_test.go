package tr064

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
