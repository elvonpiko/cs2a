package panel

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newSessionTestStore opens a throwaway panel store.
func newSessionTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// A session row is rejected once it expires, but nothing used to delete it. A
// panel left running accumulated one row per login forever, in a database the
// operator has no UI to prune.
func TestExpiredSessionsAreSweptAndRejected(t *testing.T) {
	store := newSessionTestStore(t)
	hash, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.CreateUser("admin", hash, "admin", "")
	if err != nil {
		t.Fatal(err)
	}

	live := HashToken("live-token")
	if _, err := store.CreateSession(live, u.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	dead := HashToken("dead-token")
	if _, err := store.CreateSession(dead, u.ID, -time.Hour); err != nil {
		t.Fatal(err)
	}

	if got, _ := store.GetSessionUser(dead); got != nil {
		t.Fatal("an expired session authenticated")
	}
	if got, err := store.GetSessionUser(live); err != nil || got == nil {
		t.Fatalf("a live session must authenticate: %v", err)
	}

	if err := store.DeleteExpiredSessions(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("sessions table holds %d rows, want only the live one", n)
	}
	if got, err := store.GetSessionUser(live); err != nil || got == nil {
		t.Fatalf("the live session was swept: %v", err)
	}
}

// Logging out has to invalidate the session server-side, not just clear the
// cookie: a token copied out of the browser before logout would otherwise keep
// working for the rest of its seven-day life.
func TestLogoutInvalidatesTheSessionServerSide(t *testing.T) {
	client, _, base := newPanelTest(t)

	// Create the first admin through the real setup flow.
	form := strings.NewReader("token=setuptok&username=admin&password=correct-horse")
	req, err := http.NewRequest(http.MethodPost, base+"/setup", form)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup: %d", resp.StatusCode)
	}

	req, err = http.NewRequest(http.MethodPost, base+"/login",
		strings.NewReader("username=admin&password=correct-horse"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	var token string
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no session cookie was set")
	}

	// The session works before logout.
	if code := get(t, client, base+"/").StatusCode; code != http.StatusOK {
		t.Fatalf("authenticated page before logout: %d", code)
	}

	req, err = http.NewRequest(http.MethodPost, base+"/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	var cleared *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			cleared = c
		}
	}
	if cleared == nil {
		t.Fatal("logout set no clearing cookie")
	}
	if cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Fatalf("clearing cookie = %+v", cleared)
	}
	// The clearing cookie must keep the original's attributes, or a browser
	// holding a Secure cookie does not replace it.
	if !cleared.HttpOnly || cleared.Path != "/" || cleared.SameSite != http.SameSiteLaxMode {
		t.Fatalf("clearing cookie lost its attributes: %+v", cleared)
	}

	// Replaying the stolen token must fail even though the client's jar is now
	// empty: the server-side row is gone.
	req, err = http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	bare := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err = bare.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a replayed token after logout returned %d, want a redirect to /login", resp.StatusCode)
	}
	// The redirect now carries a flash explaining the sudden login page, and
	// the dead cookie is cleared so the browser stops replaying the token.
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login") {
		t.Fatalf("Location = %q", loc)
	}
	clearedNow := false
	for _, c := range resp.Cookies() {
		if c != nil && c.Name == sessionCookie && c.Value == "" && c.MaxAge < 0 {
			clearedNow = true
		}
	}
	if !clearedNow {
		t.Fatal("the dead session cookie must be cleared in the same response")
	}
}

// Deleting a user must take their sessions with them, or a deleted admin keeps
// their open tab working until the session expires.
func TestDeletingAUserRevokesTheirSessions(t *testing.T) {
	store := newSessionTestStore(t)
	hash, err := HashPassword("correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.CreateUser("doomed", hash, "player", "")
	if err != nil {
		t.Fatal(err)
	}
	tok := HashToken("tok")
	if _, err := store.CreateSession(tok, u.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetSessionUser(tok); got != nil {
		t.Fatal("a deleted user's session still authenticates")
	}
}

// Static files are served from an embedded FS; a traversal attempt must not
// escape it. The check is cheap and the consequence (reading arbitrary files as
// root) is severe.
func TestStaticRefusesTraversal(t *testing.T) {
	client, _, base := newPanelTest(t)
	for _, path := range []string{
		"/static/../../etc/passwd",
		"/static/..%2f..%2fetc%2fpasswd",
		"/static/",
		"/static/nope.css",
	} {
		resp := get(t, client, base+path)
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s returned 200", path)
		}
	}
	// The real asset still works.
	if resp := get(t, client, base+"/static/cs2a.css"); resp.StatusCode != http.StatusOK {
		t.Fatalf("cs2a.css: %d", resp.StatusCode)
	}
}

// The login page must not accept an unauthenticated password guess without the
// throttle, and a locked-out client must be told to wait rather than silently
// rejected. 429 is the honest answer.
func TestLoginThrottleAnswers429(t *testing.T) {
	client, _, base := newPanelTest(t)
	form := strings.NewReader("token=setuptok&username=admin&password=correct-horse")
	req, _ := http.NewRequest(http.MethodPost, base+"/setup", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var last *http.Response
	for i := 0; i < throttleThreshold+1; i++ {
		req, _ := http.NewRequest(http.MethodPost, base+"/login",
			strings.NewReader("username=admin&password=wrong-guess"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("after %d failures the status was %d, want 429", throttleThreshold+1, last.StatusCode)
	}
	if last.Header.Get("Retry-After") == "" {
		t.Error("a 429 must carry Retry-After")
	}
	// The correct password is refused too while the lockout holds — otherwise
	// the throttle would be trivially bypassed by interleaving guesses.
	req, _ = http.NewRequest(http.MethodPost, base+"/login",
		strings.NewReader("username=admin&password=correct-horse"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a locked-out client got %d for the right password", resp.StatusCode)
	}
}

// Once an admin exists, /setup must be closed: a second admin created by an
// unauthenticated visitor would be a full compromise of the panel.
func TestSetupIsClosedAfterTheFirstAdmin(t *testing.T) {
	client, _, base := newPanelTest(t)
	form := strings.NewReader("token=setuptok&username=admin&password=correct-horse")
	req, _ := http.NewRequest(http.MethodPost, base+"/setup", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first setup: %d", resp.StatusCode)
	}

	// GET redirects to login.
	resp = get(t, client, base+"/setup")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("GET /setup after setup: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// POST with the same token creates nothing.
	req, _ = http.NewRequest(http.MethodPost, base+"/setup",
		strings.NewReader("token=setuptok&username=intruder&password=another-pass"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("second setup: %d", resp.StatusCode)
	}
	// Signing in as the would-be second admin must fail.
	req, _ = http.NewRequest(http.MethodPost, base+"/login",
		strings.NewReader("username=intruder&password=another-pass"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("a second admin was created after setup was complete")
	}
	if !strings.Contains(body, "Wrong username or password") {
		t.Fatalf("login body = %q", body)
	}
}

// --- sliding sessions ------------------------------------------------------
//
// A login used to live for a fixed seven days no matter what: close the
// browser, come back days later, and the tab still worked. Sessions now die
// after SessionIdle without a request, with SessionMaxAge as the hard cap,
// and an authenticated request refreshes the idle window (at most once per
// SessionTouch, so a polling page is not a write per poll).

func TestSessionIdleWindow(t *testing.T) {
	store := newSessionTestStore(t)
	hash, _ := HashPassword("correct-horse")
	u, _ := store.CreateUser("admin", hash, "admin", "")

	// a fresh session works
	tok := HashToken("fresh")
	_, _ = store.CreateSession(tok, u.ID, SessionTTL)
	if got, _ := store.GetSessionUser(tok); got == nil {
		t.Fatal("fresh session rejected")
	}

	// last_seen older than the idle window: rejected even though the absolute
	// expiry is days away
	stale := HashToken("stale")
	_, _ = store.CreateSession(stale, u.ID, SessionTTL)
	old := time.Now().UTC().Add(-(SessionIdle + time.Minute)).Format(time.RFC3339)
	if _, err := store.db.Exec(`UPDATE sessions SET last_seen = ? WHERE token_hash = ?`, old, stale); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetSessionUser(stale); got != nil {
		t.Fatal("an idle-expired session authenticated")
	}

	// and the sweeper drops the idle-dead row
	if err := store.DeleteExpiredSessions(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, stale).Scan(&n); err != nil || n != 0 {
		t.Fatalf("idle-dead session survived the sweep: n=%d err=%v", n, err)
	}
}

func TestSessionTouchRefreshesIdleWindow(t *testing.T) {
	store := newSessionTestStore(t)
	hash, _ := HashPassword("correct-horse")
	u, _ := store.CreateUser("admin", hash, "admin", "")
	tok := HashToken("touched")
	_, _ = store.CreateSession(tok, u.ID, SessionTTL)

	// last_seen just inside the idle window but older than SessionTouch: the
	// lookup must succeed AND refresh the row (and report the touch).
	ago := time.Now().UTC().Add(-(SessionTouch + time.Minute)).Format(time.RFC3339)
	if _, err := store.db.Exec(`UPDATE sessions SET last_seen = ? WHERE token_hash = ?`, ago, tok); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetSessionUser(tok); got == nil {
		t.Fatal("session inside the idle window was rejected")
	}
	if !store.SessionTouched(tok) {
		t.Fatal("lookup older than SessionTouch must report a touch")
	}
	if store.SessionTouched(tok) {
		t.Fatal("touch flag must clear once consumed")
	}
	// the row's last_seen moved: a second lookup is now a no-touch fast path
	if got, _ := store.GetSessionUser(tok); got == nil {
		t.Fatal("touched session rejected")
	}
	if store.SessionTouched(tok) {
		t.Fatal("a fresh last_seen must not touch again")
	}
}

func TestSessionAbsoluteCapBeatsActivity(t *testing.T) {
	store := newSessionTestStore(t)
	hash, _ := HashPassword("correct-horse")
	u, _ := store.CreateUser("admin", hash, "admin", "")
	tok := HashToken("ancient")
	_, _ = store.CreateSession(tok, u.ID, SessionTTL)
	// active recently (last_seen now) but created before the absolute cap:
	// expired_at is what CreateSession sets from ttl, so rewind it directly.
	ancient := time.Now().UTC().Add(-(SessionMaxAge + time.Hour)).Format(time.RFC3339)
	if _, err := store.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token_hash = ?`, ancient, tok); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetSessionUser(tok); got != nil {
		t.Fatal("a session past its absolute cap authenticated despite activity")
	}
}
