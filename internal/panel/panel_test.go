package panel

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeAgent implements the slice of the agent API the panel uses.
type fakeAgent struct {
	mu         sync.Mutex
	t          *testing.T
	statusBody string
	mapsBody   string
	actions    []string
	password   string
	changemap  string
	whitelist  []string
	wlEnabled  bool
	installs   int
	loadout    map[string][2]string
	plugins    string
}

func (f *fakeAgent) handler() http.Handler {
	return f.handlerWithRef(nil)
}

func (f *fakeAgent) handlerWithRef(ref *fakeAgent) http.Handler {
	faRef := ref
	mux := http.NewServeMux()
	check := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer agenttok" {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.Write([]byte(f.statusBody))
	})
	mux.HandleFunc("GET /api/v1/maps", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.Write([]byte(`{"maps":["de_dust2","de_mirage"]}`))
	})
	mux.HandleFunc("POST /api/v1/map", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		var req struct {
			Map string `json:"map"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if faRef != nil {
			faRef.changemap = req.Map
		}
		w.Write([]byte(`{"ok":true,"map":"` + req.Map + `"}`))
	})
	for _, action := range []string{"start", "stop", "restart"} {
		mux.HandleFunc("POST /api/v1/server/"+action, func(w http.ResponseWriter, r *http.Request) {
			if !check(w, r) {
				return
			}
			f.mu.Lock()
			f.actions = append(f.actions, strings.TrimPrefix(r.URL.Path, "/api/v1/server/"))
			f.mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		})
	}
	mux.HandleFunc("POST /api/v1/server/exec", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.Write([]byte(`{"ok":true,"output":""}`))
	})
	mux.HandleFunc("PUT /api/v1/password", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		var req struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.password = req.Password
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("PUT /api/v1/whitelist", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		var req struct {
			SteamIDs []string `json:"steamids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.whitelist = req.SteamIDs
		w.Write([]byte(`{"steamids":[]}`))
	})
	mux.HandleFunc("GET /api/v1/whitelist", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		f.mu.Lock()
		enabled := f.wlEnabled
		f.mu.Unlock()
		w.Write([]byte(`{"steamids":["76561197960287930"],"enabled":` + strconv.FormatBool(enabled) + `}`))
	})
	mux.HandleFunc("PUT /api/v1/whitelist/enabled", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		var req struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.wlEnabled = req.Enabled
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /api/v1/plugins", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.Write([]byte(f.plugins))
	})
	// installs are async: the agent answers 202 with a job, and the panel
	// polls /api/v1/jobs while the download runs
	mux.HandleFunc("POST /api/v1/plugins/weaponpaints/install", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		var req struct {
			Async bool `json:"async"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Async {
			w.Write([]byte(`{"id":"weaponpaints","version":"build-459","requires_restart":true,"installed_deps":true}`))
			return
		}
		f.mu.Lock()
		f.installs++
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"id":"job1","kind":"install","target":"weaponpaints","label":"WeaponPaints","status":"running","step":"downloading"}`))
	})
	mux.HandleFunc("GET /api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		f.mu.Lock()
		n := f.installs
		f.mu.Unlock()
		if n == 0 {
			w.Write([]byte(`{"jobs":[]}`))
			return
		}
		w.Write([]byte(`{"jobs":[{"id":"job1","kind":"install","target":"weaponpaints","label":"WeaponPaints","status":"running","step":"downloading WeaponPaints build-459"}]}`))
	})
	mux.HandleFunc("DELETE /api/v1/plugins/weaponpaints", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /api/v1/settings", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.Write([]byte(`{"settings":[{"name":"sv_password","value":"hunter2"},{"name":"sv_cheats","value":"0"}]}`))
	})
	mux.HandleFunc("PUT /api/v1/loadout/", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		steamid := strings.TrimPrefix(r.URL.Path, "/api/v1/loadout/")
		var req struct {
			Loadout struct {
				KnifeT  string `json:"knife_t"`
				KnifeCT string `json:"knife_ct"`
			} `json:"loadout"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		if f.loadout == nil {
			f.loadout = map[string][2]string{}
		}
		f.loadout[steamid] = [2]string{req.Loadout.KnifeT, req.Loadout.KnifeCT}
		f.mu.Unlock()
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /api/v1/loadout/", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		steamid := strings.TrimPrefix(r.URL.Path, "/api/v1/loadout/")
		kt, kct := "default", "default"
		if lo, ok := f.loadout[steamid]; ok {
			kt, kct = lo[0], lo[1]
		}
		w.Write([]byte(`{"loadout":{"knife_t":"` + kt + `","knife_ct":"` + kct + `"},"sync_enabled":true}`))
	})
	return mux
}

const fakeStatusRunning = `{
	"service": {"active": true, "enabled": true, "uptime_seconds": 90061},
	"info": {"name": "cs2a test server", "map": "de_dust2", "players": 2, "max": 12, "bots": 1},
	"rcon": {"hostname": "cs2a test server", "map": "de_dust2", "humans": 2, "bots": 1, "max": 12,
		"players": [
			{"user_id": "2", "name": "Alice", "steam_id": "76561197961500295", "connected": "12:34", "ping": 20, "state": "active"},
			{"user_id": "3", "name": "BOT Dave", "steam_id": "", "connected": "01:00", "ping": 0, "state": "active"}
		]}
}`

// newPanelTest builds a panel against a fake agent and returns a test client.
func newPanelTest(t *testing.T) (*http.Client, *fakeAgent, string) {
	t.Helper()
	fa := &fakeAgent{
		statusBody: fakeStatusRunning,
		plugins:    `{"plugins":[{"id":"weaponpaints","name":"WeaponPaints","description":"skins","kind":"plugin","requires":["cssharp"]},{"id":"metamod","name":"Metamod:Source","description":"loader","kind":"runtime"}]}`,
	}
	agentTS := httptest.NewServer(fa.handlerWithRef(fa))
	t.Cleanup(agentTS.Close)

	cfg := DefaultConfig()
	cfg.AgentURL = agentTS.URL
	cfg.AgentToken = "agenttok"
	cfg.DBPath = filepath.Join(t.TempDir(), "panel.db")
	cfg.Listen = "127.0.0.1:0"
	tokenFile := filepath.Join(t.TempDir(), "setup-token")
	_ = os.WriteFile(tokenFile, []byte("setuptok"), 0o600)
	cfg.SetupTokenFile = tokenFile

	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	panelTS := httptest.NewServer(NewServer(cfg, store, NewAgentClient(cfg.AgentURL, cfg.AgentToken), testLogger(t)))
	t.Cleanup(panelTS.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // don't follow; assert codes
		},
		Jar: jar,
	}
	return client, fa, panelTS.URL
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func get(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// getBody fetches a page and returns its body (get closes it).
func getBody(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return readAll(t, resp)
}

func postForm(t *testing.T, client *http.Client, url string, vals url.Values) *http.Response {
	t.Helper()
	resp, err := client.PostForm(url, vals)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func loginAs(t *testing.T, client *http.Client, base, user, pass string) {
	t.Helper()
	resp := postForm(t, client, base+"/login", url.Values{"username": {user}, "password": {pass}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login %s: want 303, got %d", user, resp.StatusCode)
	}
}

func TestPanelAuthFlow(t *testing.T) {
	client, _, base := newPanelTest(t)

	// unauthenticated root redirects to login
	if resp := get(t, client, base+"/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("root unauth: %d", resp.StatusCode)
	}

	// setup page appears (no users + token file exists)
	resp := get(t, client, base+"/setup")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup: %d", resp.StatusCode)
	}

	// bad token rejected
	if resp := postForm(t, client, base+"/setup", url.Values{"token": {"wrong"}, "username": {"admin"}, "password": {"password123"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("setup with bad token should re-render 200, got %d", resp.StatusCode)
	}

	// good token creates admin
	if resp := postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup ok: %d", resp.StatusCode)
	}

	// login with wrong password
	resp = postForm(t, client, base+"/login", url.Values{"username": {"admin"}, "password": {"nope-nope"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bad login: %d", resp.StatusCode)
	}

	// login ok
	loginAs(t, client, base, "admin", "password123")

	// root renders server page
	resp, err := client.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "cs2a test server") {
		t.Fatalf("server page: %d %q", resp.StatusCode, body[:min(200, len(body))])
	}
	if !strings.Contains(body, "de_dust2") || !strings.Contains(body, "1d 1h") {
		t.Fatalf("server page missing data")
	}
}

func TestPanelRolesAndActions(t *testing.T) {
	client, fa, base := newPanelTest(t)

	// create admin + player via store through setup & users API is tested
	// elsewhere; here create directly with a second panel store access —
	// simplest: use setup for admin, then login and create player via /users/create
	store, err := OpenStore(filepath.Join(t.TempDir(), "unused.db"))
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	// admin via setup
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	// create a player
	resp := postForm(t, client, base+"/users/create", url.Values{
		"username": {"alice"}, "password": {"alicepass1"}, "role": {"player"}, "steamid": {"[U:1:1234567]"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create player: %d", resp.StatusCode)
	}

	// whitelist the player from the access page
	resp = postForm(t, client, base+"/access/whitelist/add-user", url.Values{"user_id": {"2"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("whitelist add-user: %d", resp.StatusCode)
	}
	_ = resp
	// the fake agent seeds one pre-existing entry; our user must be appended
	if len(fa.whitelist) != 2 || fa.whitelist[1] != "76561197961500295" {
		t.Fatalf("whitelist = %v", fa.whitelist)
	}

	// admin can restart
	resp = postForm(t, client, base+"/do/restart", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("restart: %d", resp.StatusCode)
	}
	if !contains(fa.actions, "restart") {
		t.Fatalf("actions = %v", fa.actions)
	}

	// set password through the panel
	_ = postForm(t, client, base+"/access/password", url.Values{"password": {"newpw"}})
	if fa.password != "newpw" {
		t.Fatalf("password = %q", fa.password)
	}

	// player logs in on a separate client (cookies are per-client)
	pjar, _ := cookiejar.New(nil)
	pclient := &http.Client{CheckRedirect: client.CheckRedirect, Jar: pjar}
	loginAs(t, pclient, base, "alice", "alicepass1")

	// player denied admin pages
	resp = get(t, pclient, base+"/plugins")
	if resp.StatusCode != http.StatusSeeOther { // redirect to /?denied=1
		t.Fatalf("player /plugins: %d", resp.StatusCode)
	}

	// player CAN see server page with map change, but not lifecycle controls
	resp2, err := pclient.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp2)
	if !strings.Contains(body, "de_dust2") {
		t.Fatalf("player server page missing status")
	}
	if strings.Contains(body, "/do/restart") || strings.Contains(body, "/do/stop") {
		t.Fatalf("player server page must not expose admin actions")
	}
	if !strings.Contains(body, "/do/map") {
		t.Fatalf("player must be able to change map")
	}
	if resp := postForm(t, pclient, base+"/do/map", url.Values{"map": {"de_mirage"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("player map change: %d", resp.StatusCode)
	}
	if fa.changemap != "de_mirage" {
		t.Fatalf("agent changemap = %q", fa.changemap)
	}

	// player saves loadout
	resp = postForm(t, pclient, base+"/loadout", url.Values{"knife_t": {"weapon_knife_karambit"}, "knife_ct": {"weapon_bayonet"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("loadout save: %d", resp.StatusCode)
	}
	lo, ok := fa.loadout["76561197961500295"]
	if !ok || lo != [2]string{"weapon_knife_karambit", "weapon_bayonet"} {
		t.Fatalf("agent loadout = %v", fa.loadout)
	}
}

func TestPanelPluginsFlow(t *testing.T) {
	client, fa, base := newPanelTest(t)
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	resp, err := client.Get(base + "/plugins")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	if !strings.Contains(body, "WeaponPaints") || !strings.Contains(body, "Metamod:Source") {
		t.Fatalf("plugins page missing cards")
	}
	// nothing running yet: no polling attribute on the empty strip
	if strings.Contains(body, `hx-get="/partials/plugin-jobs"`) {
		t.Fatal("job polling active with no jobs")
	}

	// install returns immediately (the agent runs it as a job)
	resp = postForm(t, client, base+"/plugins/weaponpaints/install", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("install: %d", resp.StatusCode)
	}
	if fa.installs != 1 {
		t.Fatalf("agent installs = %d, want 1 async job", fa.installs)
	}

	// the page now shows live progress and polls for updates
	body = getBody(t, client, base+"/plugins")
	if !strings.Contains(body, "downloading WeaponPaints build-459") {
		t.Fatalf("install progress missing:\n%s", body[:min(600, len(body))])
	}
	if !strings.Contains(body, `hx-get="/partials/plugin-jobs"`) {
		t.Fatal("job polling not enabled while a job runs")
	}

	// the polled partial renders on its own
	body = getBody(t, client, base+"/partials/plugin-jobs")
	if !strings.Contains(body, "WeaponPaints") {
		t.Fatalf("jobs partial = %q", body)
	}
}

// Whitelist enforcement is a plugin config switch, not a cvar: the panel must
// read it from and write it to the agent's whitelist endpoints.
func TestPanelWhitelistToggle(t *testing.T) {
	client, fa, base := newPanelTest(t)
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	body := getBody(t, client, base+"/access")
	if !strings.Contains(body, "inactive") {
		t.Fatalf("access page should report whitelist inactive:\n%s", body[:min(400, len(body))])
	}

	if resp := postForm(t, client, base+"/access/whitelist/toggle", url.Values{"enabled": {"1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("toggle: %d", resp.StatusCode)
	}
	if !fa.wlEnabled {
		t.Fatal("agent whitelist not enabled")
	}
	body = getBody(t, client, base+"/access")
	if !strings.Contains(body, "enforced") {
		t.Fatal("access page should report whitelist enforced")
	}

	if resp := postForm(t, client, base+"/access/whitelist/toggle", url.Values{"enabled": {"0"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("toggle off: %d", resp.StatusCode)
	}
	if fa.wlEnabled {
		t.Fatal("agent whitelist still enabled")
	}
}

// State-changing requests from another site must be rejected, and flash
// messages containing & or + must survive the redirect intact.
func TestPanelCSRFAndFlashEscaping(t *testing.T) {
	client, _, base := newPanelTest(t)
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	req, err := http.NewRequest(http.MethodPost, base+"/do/restart", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST accepted: %d", resp.StatusCode)
	}

	// same-origin POSTs still work
	req, _ = http.NewRequest(http.MethodPost, base+"/do/restart", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("same-origin POST rejected: %d", resp.StatusCode)
	}

	// a flash with & and + must round-trip through the query string
	rec := httptest.NewRecorder()
	msg := "a&b+c d=e"
	redirectFlash(rec, httptest.NewRequest(http.MethodGet, "/", nil), "/plugins", "err", msg)
	loc := rec.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("err"); got != msg {
		t.Fatalf("flash mangled: %q -> %q (location %q)", msg, got, loc)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
