package panel

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The panel is the only internet-facing part of cs2a and authenticates with a
// password, so /login must not accept unlimited guesses.
func TestLoginIsThrottledAfterRepeatedFailures(t *testing.T) {
	client, _, base := newPanelTest(t)
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})

	// The threshold is the number of failures allowed, so attempt N+1 is the
	// first that must be refused outright.
	for i := 0; i < throttleThreshold; i++ {
		resp := postForm(t, client, base+"/login", url.Values{"username": {"admin"}, "password": {"wrongpass"}})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d: status %d, want the login page back", i+1, resp.StatusCode)
		}
	}
	resp, err := client.PostForm(base+"/login", url.Values{"username": {"admin"}, "password": {"wrongpass"}})
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt: status %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a 429 must say when to come back")
	}
	if !strings.Contains(body, "Too many failed sign-in attempts") {
		t.Errorf("the page must explain the lockout:\n%s", body)
	}

	// The correct password is refused too while the lockout holds — otherwise
	// the throttle would only slow down wrong guesses, not the guessing.
	resp = postForm(t, client, base+"/login", url.Values{"username": {"admin"}, "password": {"password123"}})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("locked out: status %d, want 429 even for the right password", resp.StatusCode)
	}
}

// A lockout must expire on its own, and a success must clear the counter, or one
// forgetful admin would be locked out of their own server for good.
func TestLoginThrottleExpiresAndResets(t *testing.T) {
	tr := newLoginThrottle()
	now := time.Now()
	tr.now = func() time.Time { return now }

	for i := 0; i < throttleThreshold; i++ {
		tr.failed("10.0.0.1", "admin")
	}
	if tr.retryAfter("10.0.0.1", "admin") <= 0 {
		t.Fatal("threshold failures must lock the key")
	}
	// An unrelated client is unaffected.
	if tr.retryAfter("10.0.0.2", "someone") != 0 {
		t.Fatal("the lockout must not spill onto other clients")
	}

	now = now.Add(throttleWindow + time.Second)
	if left := tr.retryAfter("10.0.0.1", "admin"); left != 0 {
		t.Fatalf("lockout must expire, %v left", left)
	}
	// After expiry a single mistake must not lock again immediately.
	tr.failed("10.0.0.1", "admin")
	if left := tr.retryAfter("10.0.0.1", "admin"); left != 0 {
		t.Fatalf("one failure after expiry must not re-lock, %v left", left)
	}

	// A success wipes the slate.
	for i := 0; i < throttleThreshold-1; i++ {
		tr.failed("10.0.0.1", "admin")
	}
	tr.succeeded("10.0.0.1", "admin")
	for i := 0; i < throttleThreshold-1; i++ {
		tr.failed("10.0.0.1", "admin")
	}
	if left := tr.retryAfter("10.0.0.1", "admin"); left != 0 {
		t.Fatalf("a successful login must reset the counter, %v left", left)
	}
}

// Both keys matter: an attacker rotating usernames from one host is caught by
// address, and a distributed attack on one account is caught by username.
func TestLoginThrottleCountsAddressAndUsername(t *testing.T) {
	tr := newLoginThrottle()
	now := time.Now()
	tr.now = func() time.Time { return now }

	// Same host, different usernames each time.
	for i := 0; i < throttleThreshold; i++ {
		tr.failed("10.0.0.9", "victim"+string(rune('a'+i)))
	}
	if tr.retryAfter("10.0.0.9", "fresh-name") == 0 {
		t.Error("username rotation from one address must still be throttled")
	}

	tr2 := newLoginThrottle()
	tr2.now = func() time.Time { return now }
	// Different hosts, same username.
	for i := 0; i < throttleThreshold; i++ {
		tr2.failed("10.0.1."+string(rune('1'+i)), "admin")
	}
	if tr2.retryAfter("203.0.113.7", "admin") == 0 {
		t.Error("a distributed attack on one account must still be throttled")
	}
	if tr2.retryAfter("203.0.113.7", "other") != 0 {
		t.Error("an untouched account from an untouched address must be free")
	}
}

// Behind the installer's reverse proxy every request arrives from loopback, so
// without the forwarded address one throttle key would cover the whole
// internet. The header is only trusted from loopback, so a directly exposed
// panel cannot be bypassed by sending it.
func TestClientAddrTrustsForwardedOnlyFromLoopback(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		fwd    string
		want   string
	}{
		{"proxy hop", "127.0.0.1:54321", "203.0.113.9", "203.0.113.9"},
		{"proxy chain", "127.0.0.1:54321", "203.0.113.9, 10.0.0.1", "203.0.113.9"},
		{"loopback, no header", "127.0.0.1:54321", "", "127.0.0.1"},
		{"direct client cannot spoof", "203.0.113.5:9000", "10.9.9.9", "203.0.113.5"},
		{"empty header value", "127.0.0.1:54321", " , 10.0.0.1", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := http.NewRequest("POST", "/login", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.RemoteAddr = tc.remote
			if tc.fwd != "" {
				r.Header.Set("X-Forwarded-For", tc.fwd)
			}
			if got := clientAddr(r); got != tc.want {
				t.Errorf("clientAddr = %q, want %q", got, tc.want)
			}
		})
	}
}
