package webui

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Worked example from AVM's "Session-IDs im FRITZ!Box Webinterface" document.
func TestSolveChallengeAVMExample(t *testing.T) {
	c := NewClient("http://unused", "user", "1example!", time.Second)

	got, err := c.solveChallenge("2$10000$5A1711$2000$5A1722")
	if err != nil {
		t.Fatal(err)
	}
	want := "5A1722$1798a1672bca7c6463d6b245f82b53703b0f50813401b03e4045a5861e689adb"
	if got != want {
		t.Errorf("solveChallenge() = %s, want %s", got, want)
	}
}

func TestSolveChallengeRejectsOldFormat(t *testing.T) {
	c := NewClient("http://unused", "user", "pass", time.Second)
	// Version 1 challenges (MD5) are plain hex without "$".
	if _, err := c.solveChallenge("1a2b3c4d"); err == nil {
		t.Error("expected an error for a non-PBKDF2 challenge")
	}
}

// fakeBox emulates login_sid.lua and data.lua.
type fakeBox struct {
	t        *testing.T
	user     string
	password string

	mu        sync.Mutex
	logins    int    // successful logins
	validSID  string // currently valid session
	blockTime int    // returned in BlockTime
}

const (
	testChallenge = "2$1000$5A1711$100$5A1722" // few iterations: fast tests
	testPayload   = `{"pid":"docInfo","sid":"%s","data":{}}`
)

func (f *fakeBox) expectedResponse() string {
	salt1, _ := hex.DecodeString("5A1711")
	salt2, _ := hex.DecodeString("5A1722")
	h1, _ := pbkdf2.Key(sha256.New, f.password, salt1, 1000, sha256.Size)
	h2, _ := pbkdf2.Key(sha256.New, string(h1), salt2, 100, sha256.Size)
	return "5A1722$" + hex.EncodeToString(h2)
}

func (f *fakeBox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()

	switch r.URL.Path {
	case "/login_sid.lua":
		sid := emptySID
		if r.Method == http.MethodPost {
			if r.PostForm.Get("username") == f.user && r.PostForm.Get("response") == f.expectedResponse() {
				f.logins++
				f.validSID = fmt.Sprintf("%016d", f.logins)
				sid = f.validSID
			}
		}
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><SessionInfo><SID>%s</SID>`+
			`<Challenge>%s</Challenge><BlockTime>%d</BlockTime></SessionInfo>`, sid, testChallenge, f.blockTime)

	case "/data.lua":
		if f.validSID == "" || r.PostForm.Get("sid") != f.validSID {
			// Like the real box: an invalid SID gets the HTML login page.
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, "<html>login</html>")
			return
		}
		if got := r.PostForm.Get("page"); got != "docInfo" {
			f.t.Errorf("page = %q, want docInfo", got)
		}
		_, _ = fmt.Fprintf(w, testPayload, f.validSID)

	default:
		http.NotFound(w, r)
	}
}

func newFakeBox(t *testing.T) (*fakeBox, *httptest.Server) {
	t.Helper()
	box := &fakeBox{t: t, user: "prometheus", password: "secret"}
	srv := httptest.NewServer(box)
	t.Cleanup(srv.Close)
	return box, srv
}

func TestDataReusesSession(t *testing.T) {
	box, srv := newFakeBox(t)
	c := NewClient(srv.URL, "prometheus", "secret", 5*time.Second)

	for i := range 3 {
		if _, err := c.Data(context.Background(), "docInfo"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if box.logins != 1 {
		t.Errorf("logins = %d, want 1 (session should be reused)", box.logins)
	}
}

func TestDataRelogsInAfterSessionExpired(t *testing.T) {
	box, srv := newFakeBox(t)
	c := NewClient(srv.URL, "prometheus", "secret", 5*time.Second)

	if _, err := c.Data(context.Background(), "docInfo"); err != nil {
		t.Fatal(err)
	}

	// The box drops the session, e.g. after 20 minutes of inactivity.
	box.mu.Lock()
	box.validSID = ""
	box.mu.Unlock()

	if _, err := c.Data(context.Background(), "docInfo"); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if box.logins != 2 {
		t.Errorf("logins = %d, want 2", box.logins)
	}
}

func TestDataWrongPassword(t *testing.T) {
	_, srv := newFakeBox(t)
	c := NewClient(srv.URL, "prometheus", "wrong", 5*time.Second)

	_, err := c.Data(context.Background(), "docInfo")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}

func TestDataLoginBlocked(t *testing.T) {
	box, srv := newFakeBox(t)
	box.blockTime = 32
	c := NewClient(srv.URL, "prometheus", "secret", 5*time.Second)

	_, err := c.Data(context.Background(), "docInfo")

	var blocked *BlockedError
	if !errors.As(err, &blocked) || blocked.Seconds != 32 {
		t.Fatalf("error = %v, want BlockedError{32}", err)
	}
	if box.logins != 0 {
		t.Error("client must not try to log in while blocked")
	}
}
