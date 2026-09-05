package panel

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginThrottle slows down password guessing against the panel.
//
// The panel is the one internet-facing part of cs2a and it authenticates with a
// username and password, so an unthrottled /login is an open invitation: bcrypt
// cost 10 is roughly 60 ms, which still allows thousands of guesses per hour
// per connection and many more in parallel.
//
// The design is deliberately small: no store, no cleanup goroutine, no external
// dependency. Failures are counted per client address and per username (an
// attacker rotating usernames from one host is throttled by address, and a
// botnet hammering one account is throttled by username). After
// throttleThreshold failures the key is locked for throttleWindow, and every
// further attempt while locked extends nothing — it just answers immediately
// with the time left, so a lockout cannot be turned into a denial of service
// against the real operator by keeping their account permanently locked: the
// window is short and a correct password clears the counter.
type loginThrottle struct {
	mu   sync.Mutex
	fail map[string]*failState
	// now is injectable so tests do not sleep.
	now func() time.Time
}

type failState struct {
	count int
	// until is when the current lockout expires (zero when not locked).
	until time.Time
	// seen is the last time this key was touched, used to expire idle entries
	// instead of growing the map forever.
	seen time.Time
}

const (
	// throttleThreshold is how many failures a key may accumulate before it is
	// locked out. Five is comfortably above honest typos.
	throttleThreshold = 5
	// throttleWindow is how long a locked key stays locked.
	throttleWindow = 5 * time.Minute
	// throttleIdle is when an untouched entry is dropped from the map.
	throttleIdle = 30 * time.Minute
	// throttleMaxKeys bounds memory if someone sprays random usernames.
	throttleMaxKeys = 4096
)

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{fail: make(map[string]*failState), now: time.Now}
}

// keysFor returns the throttle keys a login attempt is counted against.
func keysFor(addr, username string) []string {
	return []string{"ip:" + addr, "user:" + strings.ToLower(username)}
}

// retryAfter reports how long the caller must wait, or 0 when the attempt may
// proceed.
func (t *loginThrottle) retryAfter(addr, username string) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	var worst time.Duration
	for _, k := range keysFor(addr, username) {
		st := t.fail[k]
		if st == nil || st.until.IsZero() {
			continue
		}
		if left := st.until.Sub(now); left > worst {
			worst = left
		}
	}
	return worst
}

// fail records a failed attempt and locks the keys once the threshold is hit.
func (t *loginThrottle) failed(addr, username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweep(now)
	for _, k := range keysFor(addr, username) {
		st := t.fail[k]
		if st == nil {
			if len(t.fail) >= throttleMaxKeys {
				continue // full: existing lockouts still apply
			}
			st = &failState{}
			t.fail[k] = st
		}
		// A lockout that has expired starts the count over rather than locking
		// again on the next single mistake.
		if !st.until.IsZero() && now.After(st.until) {
			st.count = 0
			st.until = time.Time{}
		}
		st.count++
		st.seen = now
		if st.count >= throttleThreshold {
			st.until = now.Add(throttleWindow)
		}
	}
}

// succeeded clears the counters for a successful login.
func (t *loginThrottle) succeeded(addr, username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keysFor(addr, username) {
		delete(t.fail, k)
	}
}

// sweep drops entries nobody has touched recently. Called on failure, which is
// the only path that grows the map.
func (t *loginThrottle) sweep(now time.Time) {
	for k, st := range t.fail {
		if now.Sub(st.seen) > throttleIdle && (st.until.IsZero() || now.After(st.until)) {
			delete(t.fail, k)
		}
	}
}

// clientAddr is the address a request is throttled against. Behind the reverse
// proxy the installer configures, every request arrives from loopback with the
// real client in X-Forwarded-For; that header is only trusted when the direct
// peer is loopback, so a public bind cannot be bypassed by sending the header.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return host
	}
	fwd := r.Header.Get("X-Forwarded-For")
	if fwd == "" {
		return host
	}
	// Left-most entry is the original client; the proxy appends, so the last
	// entry is the hop we already know about.
	first := strings.TrimSpace(strings.Split(fwd, ",")[0])
	if first == "" {
		return host
	}
	return first
}
