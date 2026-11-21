package tr064

import (
	"crypto/md5" //nolint:gosec // MD5 is mandated by HTTP digest auth (RFC 2617).
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// maxReuseRejections is how often in a row a reused nonce may be rejected
// before nonce reuse is switched off. Some servers accept every nonce only
// once; for them every reuse attempt is a rejected login, which the box may
// count as a failed attempt and answer with a login block.
const maxReuseRejections = 3

// DigestTransport is an http.RoundTripper that adds HTTP digest
// authentication (RFC 2617, algorithm MD5, qop=auth) to requests.
//
// The first request is sent without credentials. The server answers with
// 401 and a digest challenge, and the request is sent again with a matching
// Authorization header. The challenge is then cached: later requests reuse
// its nonce with an increasing nonce count (nc) and are authenticated right
// away, which saves one round trip per request. If the server rejects a
// reused nonce, the transport answers the new challenge; after
// maxReuseRejections rejections in a row it stops reusing nonces.
//
// A DigestTransport must not be copied after first use.
type DigestTransport struct {
	Username string
	Password string

	// Next is the transport that actually sends the requests.
	// http.DefaultTransport is used if nil.
	Next http.RoundTripper

	// newCnonce creates the client nonce. Tests replace it to get
	// deterministic headers; nil means crypto/rand.
	newCnonce func() string

	mu         sync.Mutex // guards the fields below
	cached     *challenge // last challenge, reused for later requests
	nc         uint32     // nonce count for cached
	rejections int        // reused nonces rejected in a row
}

// RoundTrip implements http.RoundTripper.
func (t *DigestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.Next
	if next == nil {
		next = http.DefaultTransport
	}

	// A request may be sent twice, so its body must be replayable.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return nil, errors.New("digest auth: request body cannot be replayed (GetBody is nil)")
	}

	uri := req.URL.RequestURI()

	// Fast path: authenticate right away with the cached challenge.
	if auth, ok := t.reuseAuthorization(req.Method, uri); ok {
		resp, err := t.send(next, req, auth, true)
		if err != nil || resp.StatusCode != http.StatusUnauthorized {
			if err == nil {
				t.reuseAccepted()
			}
			return resp, err
		}
		// Nonce expired, single-use or wrong password: answer the new challenge.
		t.reuseRejected()
		return t.answerChallenge(next, req, resp)
	}

	resp, err := t.send(next, req, "", true)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	return t.answerChallenge(next, req, resp)
}

// answerChallenge reads the challenge from a 401 response, caches it and
// sends the request again with credentials.
func (t *DigestTransport) answerChallenge(next http.RoundTripper, req *http.Request, resp *http.Response) (*http.Response, error) {
	header := resp.Header.Get("WWW-Authenticate")
	// Drain and close the 401 response so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	ch, err := parseChallenge(header)
	if err != nil {
		return nil, err
	}

	auth := t.newChallenge(ch, req.Method, req.URL.RequestURI())
	return t.send(next, req, auth, false)
}

// send sends req with the given Authorization header (none if empty).
// first is true for the first attempt, which may still use req's body;
// later attempts need a fresh body from GetBody.
func (t *DigestTransport) send(next http.RoundTripper, req *http.Request, auth string, first bool) (*http.Response, error) {
	out := req
	if auth != "" || !first {
		// A RoundTripper must not modify the caller's request.
		out = req.Clone(req.Context())
	}
	if !first && req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("digest auth: replay request body: %w", err)
		}
		out.Body = body
	}
	if auth != "" {
		out.Header.Set("Authorization", auth)
	}
	return next.RoundTrip(out)
}

// reuseAuthorization returns an Authorization header for the cached
// challenge with the next nonce count, if reuse is possible.
func (t *DigestTransport) reuseAuthorization(method, uri string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.cached == nil || t.rejections >= maxReuseRejections {
		return "", false
	}
	t.nc++
	return t.authorization(*t.cached, t.nc, method, uri), true
}

// newChallenge caches ch and returns the Authorization header for its first use.
func (t *DigestTransport) newChallenge(ch challenge, method, uri string) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.cached = &ch
	t.nc = 1
	return t.authorization(ch, t.nc, method, uri)
}

func (t *DigestTransport) reuseAccepted() {
	t.mu.Lock()
	t.rejections = 0
	t.mu.Unlock()
}

func (t *DigestTransport) reuseRejected() {
	t.mu.Lock()
	t.rejections++
	t.mu.Unlock()
}

// authorization builds the value of the Authorization header.
func (t *DigestTransport) authorization(ch challenge, nc uint32, method, uri string) string {
	qop := ""
	if ch.qop != "" {
		qop = "auth"
	}
	ncHex := fmt.Sprintf("%08x", nc)
	cnonce := t.cnonce()
	response := digestResponse(t.Username, t.Password, ch.realm, method, uri, ch.nonce, ncHex, cnonce, qop)

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username=%s, realm=%s, nonce=%s, uri=%s, response=%s`,
		quote(t.Username), quote(ch.realm), quote(ch.nonce), quote(uri), quote(response))
	if ch.algorithm != "" {
		fmt.Fprintf(&b, `, algorithm=%s`, ch.algorithm)
	}
	if qop != "" {
		fmt.Fprintf(&b, `, qop=%s, nc=%s, cnonce=%s`, qop, ncHex, quote(cnonce))
	}
	if ch.opaque != "" {
		fmt.Fprintf(&b, `, opaque=%s`, quote(ch.opaque))
	}
	return b.String()
}

// quoteEscaper escapes the characters that would end a quoted-string early.
var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// quote returns s as an HTTP quoted-string (RFC 7230), so a username or a
// server value containing `"` cannot break the header.
func quote(s string) string {
	return `"` + quoteEscaper.Replace(s) + `"`
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
	sum := md5.Sum([]byte(s)) //nolint:gosec // MD5 is mandated by RFC 2617
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
