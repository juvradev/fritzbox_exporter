// Package webui is a minimal client for the FRITZ!Box web interface.
//
// Some data, like the DOCSIS channel values of cable boxes, is not available
// via TR-064. It can only be read from the web UI's data.lua endpoint, which
// needs a session ID (SID) from the PBKDF2 challenge-response login at
// login_sid.lua.
package webui

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// emptySID is what the box returns when there is no valid session.
const emptySID = "0000000000000000"

// maxResponseSize protects against unexpectedly large responses.
const maxResponseSize = 4 << 20 // 4 MiB

var (
	// ErrUnauthorized is returned when the box rejects the credentials.
	ErrUnauthorized = errors.New("web ui login failed: check username, password and user rights")

	// errSessionExpired means the SID is no longer valid and a new login is needed.
	errSessionExpired = errors.New("web ui session expired")
)

// BlockedError is returned while the box refuses logins after failed attempts.
type BlockedError struct {
	Seconds int
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("web ui login blocked for %d seconds after failed attempts", e.Seconds)
}

// Client reads pages from the web UI. It logs in once and reuses the SID
// until the box invalidates it (after 20 minutes without requests).
// It is safe for concurrent use.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client

	mu     sync.Mutex // guards the fields below
	sid    string
	stage1 stage1Cache
}

// stage1Cache keeps the expensive first PBKDF2 hash. It only depends on the
// password, salt1 and iter1, which stay the same between logins.
type stage1Cache struct {
	salt1 string
	iter1 int
	hash  []byte
}

// NewClient creates a client for baseURL (e.g. "http://192.168.178.1").
func NewClient(baseURL, username, password string, timeout time.Duration) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http:     &http.Client{Timeout: timeout},
	}
}

// Data fetches a data.lua page (e.g. "docInfo") and returns the raw JSON.
// It logs in if needed and retries once with a fresh session if the SID
// has expired.
func (c *Client) Data(ctx context.Context, page string) ([]byte, error) {
	// One request at a time: concurrent logins would invalidate each other.
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sid == "" {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
	}

	body, err := c.fetch(ctx, page)
	if errors.Is(err, errSessionExpired) {
		if err := c.login(ctx); err != nil {
			return nil, err
		}
		body, err = c.fetch(ctx, page)
	}
	return body, err
}

// fetch requests a page with the current SID.
func (c *Client) fetch(ctx context.Context, page string) ([]byte, error) {
	form := url.Values{
		"sid":   {c.sid},
		"page":  {page},
		"xhr":   {"1"},
		"xhrId": {"all"},
		"lang":  {"de"},
	}
	body, status, err := c.post(ctx, "/data.lua", form)
	if err != nil {
		return nil, fmt.Errorf("data.lua %s: %w", page, err)
	}
	if status == http.StatusForbidden {
		return nil, errSessionExpired
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("data.lua %s: unexpected HTTP status %d", page, status)
	}

	// A valid answer is JSON with the (possibly renewed) SID. An expired
	// session returns the HTML login page or an empty SID instead.
	var head struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal(body, &head); err != nil || head.SID == emptySID {
		c.sid = ""
		return nil, errSessionExpired
	}
	if head.SID != "" {
		c.sid = head.SID
	}
	return body, nil
}

// sessionInfo mirrors the XML returned by login_sid.lua.
type sessionInfo struct {
	SID       string `xml:"SID"`
	Challenge string `xml:"Challenge"`
	BlockTime int    `xml:"BlockTime"`
}

// login performs the PBKDF2 challenge-response login and stores the SID.
func (c *Client) login(ctx context.Context) error {
	c.sid = ""

	info, err := c.sessionInfo(ctx, nil)
	if err != nil {
		return fmt.Errorf("fetch login challenge: %w", err)
	}
	if info.BlockTime > 0 {
		return &BlockedError{Seconds: info.BlockTime}
	}

	response, err := c.solveChallenge(info.Challenge)
	if err != nil {
		return err
	}

	info, err = c.sessionInfo(ctx, url.Values{
		"username": {c.username},
		"response": {response},
	})
	if err != nil {
		return fmt.Errorf("send login response: %w", err)
	}
	if info.SID == "" || info.SID == emptySID {
		return ErrUnauthorized
	}

	c.sid = info.SID
	return nil
}

// sessionInfo calls login_sid.lua: a GET without form returns a challenge,
// a POST with username and response returns the SID.
func (c *Client) sessionInfo(ctx context.Context, form url.Values) (sessionInfo, error) {
	var (
		body   []byte
		status int
		err    error
	)
	if form == nil {
		body, status, err = c.get(ctx, "/login_sid.lua?version=2")
	} else {
		body, status, err = c.post(ctx, "/login_sid.lua?version=2", form)
	}
	if err != nil {
		return sessionInfo{}, err
	}
	if status != http.StatusOK {
		return sessionInfo{}, fmt.Errorf("unexpected HTTP status %d", status)
	}

	var info sessionInfo
	if err := xml.Unmarshal(body, &info); err != nil {
		return sessionInfo{}, fmt.Errorf("decode session info: %w", err)
	}
	return info, nil
}

// solveChallenge computes the response for a version 2 (PBKDF2) challenge.
// Format: 2$<iter1>$<salt1>$<iter2>$<salt2>
func (c *Client) solveChallenge(challenge string) (string, error) {
	parts := strings.Split(challenge, "$")
	if len(parts) != 5 || parts[0] != "2" {
		return "", fmt.Errorf("unsupported login challenge %q (expected PBKDF2 version 2)", challenge)
	}

	iter1, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", fmt.Errorf("parse iter1: %w", err)
	}
	iter2, err := strconv.Atoi(parts[3])
	if err != nil {
		return "", fmt.Errorf("parse iter2: %w", err)
	}
	salt2, err := hex.DecodeString(parts[4])
	if err != nil {
		return "", fmt.Errorf("decode salt2: %w", err)
	}

	hash1, err := c.stage1Hash(parts[2], iter1)
	if err != nil {
		return "", err
	}
	// Stage 2: the raw bytes of hash1 are used as the "password".
	hash2, err := pbkdf2.Key(sha256.New, string(hash1), salt2, iter2, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("pbkdf2 stage 2: %w", err)
	}

	// salt2 is sent back exactly as received (hex).
	return parts[4] + "$" + hex.EncodeToString(hash2), nil
}

// stage1Hash returns PBKDF2(password, salt1, iter1), cached because it is
// the expensive part of the login (tens of thousands of iterations).
func (c *Client) stage1Hash(salt1Hex string, iter1 int) ([]byte, error) {
	if c.stage1.hash != nil && c.stage1.salt1 == salt1Hex && c.stage1.iter1 == iter1 {
		return c.stage1.hash, nil
	}

	salt1, err := hex.DecodeString(salt1Hex)
	if err != nil {
		return nil, fmt.Errorf("decode salt1: %w", err)
	}
	hash, err := pbkdf2.Key(sha256.New, c.password, salt1, iter1, sha256.Size)
	if err != nil {
		return nil, fmt.Errorf("pbkdf2 stage 1: %w", err)
	}

	c.stage1 = stage1Cache{salt1: salt1Hex, iter1: iter1, hash: hash}
	return hash, nil
}

func (c *Client) get(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	return c.do(req)
}

func (c *Client) post(ctx context.Context, path string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
}

func (c *Client) do(req *http.Request) ([]byte, int, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %w", err)
	}
	return body, resp.StatusCode, nil
}
