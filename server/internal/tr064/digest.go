package tr064

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// authentication (RFC 2617, algorithm MD5, qop=auth) to requests.
//
// Every request is first sent without credentials. If the server answers
// with 401 and a digest challenge, the request is sent once more with a
// matching Authorization header.
type DigestTransport struct {
	Username string
	Password string

	// Next is the transport that actually sends the requests.
	// http.DefaultTransport is used if nil.
	Next http.RoundTripper

	// newCnonce creates the client nonce. Tests replace it to get
	// deterministic headers; nil means crypto/rand.
	newCnonce func() string
}

// nonceCount is always 1 because every challenge is used exactly once.
const nonceCount = "00000001"

// RoundTrip implements http.RoundTripper.
func (t *DigestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}

	// The first attempt consumes the body, so the retry needs a fresh copy.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return nil, errors.New("digest auth: request body cannot be replayed (GetBody is nil)")
	}

	resp, err := next.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	header := resp.Header.Get("WWW-Authenticate")
	// Drain and close the 401 response so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	ch, err := parseChallenge(header)
	if err != nil {
		return nil, err
	}

	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("digest auth: replay request body: %w", err)
		}
		retry.Body = body
	}
	retry.Header.Set("Authorization", t.authorization(ch, req.Method, req.URL.RequestURI()))

	return next.RoundTrip(retry)
}

// authorization builds the value of the Authorization header.
func (t *DigestTransport) authorization(ch challenge, method, uri string) string {
	qop := ""
	if ch.qop != "" {
		qop = "auth"
	}
	cnonce := t.cnonce()
	response := digestResponse(t.Username, t.Password, ch.realm, method, uri, ch.nonce, nonceCount, cnonce, qop)

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`,
		t.Username, ch.realm, ch.nonce, uri, response)
	if ch.algorithm != "" {
		fmt.Fprintf(&b, `, algorithm=%s`, ch.algorithm)
	}
	if qop != "" {
		fmt.Fprintf(&b, `, qop=%s, nc=%s, cnonce="%s"`, qop, nonceCount, cnonce)
	}
	if ch.opaque != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, ch.opaque)
	}
	return b.String()
}

func (t *DigestTransport) cnonce() string {
	if t.newCnonce != nil {
		return t.newCnonce()
	}
	return rand.Text()
}

// digestResponse computes the "response" field as defined in RFC 2617.
// If qop is empty, the legacy RFC 2069 formula is used.
func digestResponse(username, password, realm, method, uri, nonce, nc, cnonce, qop string) string {
	ha1 := md5Hex(username + ":" + realm + ":" + password)
	ha2 := md5Hex(method + ":" + uri)
	if qop == "" {
		return md5Hex(ha1 + ":" + nonce + ":" + ha2)
	}
	return md5Hex(strings.Join([]string{ha1, nonce, nc, cnonce, qop, ha2}, ":"))
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // see import comment
	return hex.EncodeToString(sum[:])
}

// challenge holds the parameters of a WWW-Authenticate: Digest header.
type challenge struct {
	realm     string
	nonce     string
	qop       string
	algorithm string
	opaque    string
}

// parseChallenge parses a header like:
//
//	Digest realm="F!Box SOAP-Auth", nonce="ABC", algorithm=MD5, qop="auth"
func parseChallenge(header string) (challenge, error) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	if !strings.EqualFold(scheme, "Digest") {
		return challenge{}, fmt.Errorf("digest auth: unsupported challenge %q", header)
	}

	p := parseParams(rest)
	ch := challenge{
		realm:     p["realm"],
		nonce:     p["nonce"],
		qop:       p["qop"],
		algorithm: p["algorithm"],
		opaque:    p["opaque"],
	}

	switch {
	case ch.nonce == "":
		return challenge{}, errors.New("digest auth: challenge has no nonce")
	case ch.algorithm != "" && !strings.EqualFold(ch.algorithm, "MD5"):
		return challenge{}, fmt.Errorf("digest auth: unsupported algorithm %q", ch.algorithm)
	case ch.qop != "" && !hasToken(ch.qop, "auth"):
		return challenge{}, fmt.Errorf("digest auth: unsupported qop %q", ch.qop)
	}
	return ch, nil
}

// parseParams splits `key=value, key="quoted, value"` into a map.
// Keys are lower-cased. Escaped quotes inside values are not supported.
func parseParams(s string) map[string]string {
	params := make(map[string]string)
	for {
		s = strings.TrimLeft(s, " \t,")
		if s == "" {
			return params
		}

		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			return params
		}
		key = strings.ToLower(strings.TrimSpace(key))

		var value string
		if strings.HasPrefix(rest, `"`) {
			// Quoted value: read up to the closing quote, commas included.
			rest = rest[1:]
			end := strings.IndexByte(rest, '"')
			if end < 0 {
				end = len(rest)
			}
			value = rest[:end]
			rest = rest[min(end+1, len(rest)):]
		} else {
			// Token value: read up to the next comma.
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			value = strings.TrimSpace(rest[:end])
			rest = rest[end:]
		}

		params[key] = value
		s = rest
	}
}

// hasToken reports whether a comma-separated list contains token.
func hasToken(list, token string) bool {
	for item := range strings.SplitSeq(list, ",") {
		if strings.EqualFold(strings.TrimSpace(item), token) {
			return true
		}
	}
	return false
}
