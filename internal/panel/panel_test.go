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
	// actionFails makes lifecycle actions report a unit that would not start.
	actionFails bool
	// repairs counts rcon-repair calls.
	repairs int
	// jobsBody overrides the /api/v1/jobs response when set.
	jobsBody string
	// loadoutSyncOff makes the agent report that WeaponPaints sync is not set
	// up, so a saved loadout cannot reach the game.
	loadoutSyncOff bool
	// wlEmpty makes the agent report an empty whitelist.
	wlEmpty bool
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
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/server/")
			f.mu.Lock()
			f.actions = append(f.actions, name)
			fail := f.actionFails
			f.mu.Unlock()
			if fail {
				// The agent verifies the unit settled; a unit that dies right
				// after start comes back as 409 with the journal attached.
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"action":"` + name + `","active":false,"failed":true,` +
					`"message":"The server did not stay running — it exited right after starting.",` +
					`"log":["Sep 05 12:00:00 vps cs2-server[42]: Fatal: could not bind port 27015"]}`))
				return
			}
			w.Write([]byte(`{"action":"` + name + `","active":` +
				strconv.FormatBool(name != "stop") + `,"sub":"running","message":"Server is running."}`))
		})
	}
	mux.HandleFunc("GET /api/v1/server/logs", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.Write([]byte(`{"lines":["Sep 05 12:00:00 vps cs2-server[42]: Server is hibernating"]}`))
	})
	mux.HandleFunc("POST /api/v1/server/rcon-repair", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		f.mu.Lock()
		f.repairs++
		f.mu.Unlock()
		w.Write([]byte(`{"applied":["wrote rcon_password into server.cfg","added -usercon to the launch line"],` +
			`"result":{"action":"restart","active":true,"message":"Server is running."},"rcon":{"ok":true}}`))
	})
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
		empty := f.wlEmpty
		list := append([]string(nil), f.whitelist...)
		f.mu.Unlock()
		if len(list) == 0 {
			list = []string{"76561197960287930"}
		}
		if empty {
			list = nil
		}
		body, _ := json.Marshal(map[string]any{"steamids": list, "enabled": enabled})
		w.Write(body)
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
		body := f.jobsBody
		f.mu.Unlock()
		if body != "" {
			w.Write([]byte(body))
			return
		}
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
		syncOn := !f.loadoutSyncOff
		f.mu.Unlock()
		if syncOn {
			w.Write([]byte(`{"ok":true,"sync_enabled":true}`))
			return
		}
		w.Write([]byte(`{"ok":true,"sync_enabled":false}`))
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
	return newPanelTestWithJobs(t, "")
}

// newPanelTestWithJobs is newPanelTest with a canned /api/v1/jobs response.
func newPanelTestWithJobs(t *testing.T, jobsBody string) (*http.Client, *fakeAgent, string) {
	t.Helper()
	fa := &fakeAgent{
		statusBody: fakeStatusRunning,
		plugins:    `{"plugins":[{"id":"weaponpaints","name":"WeaponPaints","description":"skins","kind":"plugin","requires":["cssharp"]},{"id":"metamod","name":"Metamod:Source","description":"loader","kind":"runtime"}]}`,
		jobsBody:   jobsBody,
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

// newPlayerClient returns a second client with its own cookie jar, for tests
// that need an admin and a player session at the same time.
func newPlayerClient(t *testing.T, like *http.Client) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	return &http.Client{CheckRedirect: like.CheckRedirect, Jar: jar}
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
	// Sync is on in the fake agent, so the confirmation may promise in-game effect.
	if body := getBody(t, pclient, base+resp.Header.Get("Location")); !strings.Contains(body, "applies when you (re)connect") {
		t.Fatalf("loadout confirmation = %q", body[:min(1200, len(body))])
	}
}

// Saving a loadout while WeaponPaints has no database must not promise it will
// show up in game: nothing reaches the game server at all in that state.
func TestLoadoutSaveWarnsWhenSyncIsOff(t *testing.T) {
	client, fa, base := newPanelTest(t)
	fa.loadoutSyncOff = true
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{
		"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"},
		"steamid": {"76561197961500295"},
	})
	loginAs(t, client, base, "admin", "password123")

	resp := postForm(t, client, base+"/loadout", url.Values{"knife_t": {"weapon_knife_karambit"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("loadout save: %d", resp.StatusCode)
	}
	body := getBody(t, client, base+resp.Header.Get("Location"))
	if !strings.Contains(body, "skins sync is not set up yet") {
		t.Fatalf("the player must be told the save cannot reach the game:\n%s", body[:min(1500, len(body))])
	}
	if strings.Contains(body, "applies when you (re)connect") {
		t.Fatal("must not promise in-game effect without sync")
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

// Enforcing an empty whitelist rejects every player, including the admin who
// clicked the switch. The page said so in prose; the click must be refused.
func TestWhitelistCannotBeEnforcedWhileEmpty(t *testing.T) {
	client, fa, base := newPanelTest(t)
	fa.wlEmpty = true
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	resp := postForm(t, client, base+"/access/whitelist/toggle", url.Values{"enabled": {"1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("toggle: %d", resp.StatusCode)
	}
	if fa.wlEnabled {
		t.Fatal("enforcement must not be enabled with an empty list")
	}
	body := getBody(t, client, base+resp.Header.Get("Location"))
	if !strings.Contains(body, "Add at least one SteamID") {
		t.Fatalf("the refusal must be explained:\n%s", body[:min(1200, len(body))])
	}

	// Turning enforcement off is always allowed, even with an empty list.
	if resp := postForm(t, client, base+"/access/whitelist/toggle", url.Values{"enabled": {"0"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("toggle off: %d", resp.StatusCode)
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

// The status card owns its own polling attribute. The page used to wrap it in a
// second polling container, so every 5 s tick fired two requests and nested one
// card inside the other.
func TestServerPagePollsStatusCardOnce(t *testing.T) {
	client, _, base := newPanelTest(t)
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	body := getBody(t, client, base+"/")
	if n := strings.Count(body, `hx-get="/partials/status-card"`); n != 1 {
		t.Fatalf("status card polls %d times per tick, want 1", n)
	}
	if n := strings.Count(body, `id="status-card"`); n != 1 {
		t.Fatalf("status card rendered %d times", n)
	}
	// The partial must be self-sufficient: htmx swaps it in as the whole card.
	partial := getBody(t, client, base+"/partials/status-card")
	if strings.Count(partial, `hx-get="/partials/status-card"`) != 1 {
		t.Fatalf("partial lost its polling attribute:\n%s", partial)
	}
}

// The RCON failure the user hit must reach the page as an explanation plus a
// one-click repair, not as "dial 127.0.0.1:27015: connect: connection refused".
func TestServerPageExplainsRCONProblem(t *testing.T) {
	client, fa, base := newPanelTest(t)
	fa.statusBody = `{
		"service": {"active": true, "enabled": true, "uptime_seconds": 120},
		"info": {"name": "cs2a test server", "map": "de_dust2", "players": 0, "max": 12},
		"note": "rcon: the game server's launch line has no -usercon, so RCON is disabled in the engine",
		"diag": {"ok": false, "addr": "127.0.0.1:27015",
			"reason": "the game server's launch line has no -usercon, so RCON is disabled in the engine",
			"fix": "add -usercon to the launch line and restart the server",
			"repairable": true, "missing_usercon": true}
	}`
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	body := getBody(t, client, base+"/")
	if !strings.Contains(body, "no -usercon") {
		t.Fatalf("page does not explain the problem:\n%s", body[:min(1200, len(body))])
	}
	if !strings.Contains(body, "/do/rcon-repair") {
		t.Fatal("a repairable problem must offer the fix button")
	}
	// The raw socket error must not be what the operator reads.
	if strings.Contains(body, "connection refused") {
		t.Fatal("raw dial error leaked into the page")
	}

	if resp := postForm(t, client, base+"/do/rcon-repair", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("repair: %d", resp.StatusCode)
	}
	if fa.repairs != 1 {
		t.Fatalf("agent repairs = %d", fa.repairs)
	}
}

// A player must not be offered a repair button that posts to an admin route.
func TestRCONRepairIsAdminOnly(t *testing.T) {
	client, fa, base := newPanelTest(t)
	fa.statusBody = `{"service":{"active":true},"diag":{"ok":false,"reason":"x","fix":"y","repairable":true}}`
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")
	if resp := postForm(t, client, base+"/users/create", url.Values{
		"username": {"bob"}, "password": {"bobpass123"}, "role": {"player"},
	}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create player: %d", resp.StatusCode)
	}

	pjar, _ := cookiejar.New(nil)
	pclient := &http.Client{CheckRedirect: client.CheckRedirect, Jar: pjar}
	loginAs(t, pclient, base, "bob", "bobpass123")

	body := getBody(t, pclient, base+"/")
	if strings.Contains(body, "/do/rcon-repair") {
		t.Fatal("players must not see the repair button")
	}
	if resp := postForm(t, pclient, base+"/do/rcon-repair", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("player repair should be redirected away: %d", resp.StatusCode)
	}
	if fa.repairs != 0 {
		t.Fatal("a player triggered an admin repair")
	}
}

// A start that does not stick must say so and quote the journal. The buttons
// previously reported success for a server that had already died.
func TestServerActionReportsFailureWithLog(t *testing.T) {
	client, fa, base := newPanelTest(t)
	fa.actionFails = true
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	resp := postForm(t, client, base+"/do/start", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	msg := loc.Query().Get("err")
	if msg == "" {
		t.Fatalf("a failed start must flash an error, got %q", resp.Header.Get("Location"))
	}
	if !strings.Contains(msg, "did not stay running") {
		t.Fatalf("flash = %q", msg)
	}
	if !strings.Contains(msg, "could not bind port 27015") {
		t.Fatalf("journal line missing from the flash: %q", msg)
	}
}

// The admin server page shows the journal so a failing server can be diagnosed
// without SSH; players do not get it.
func TestServerPageShowsLogsForAdminsOnly(t *testing.T) {
	client, _, base := newPanelTest(t)
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")
	if resp := postForm(t, client, base+"/users/create", url.Values{
		"username": {"carol"}, "password": {"carolpass1"}, "role": {"player"},
	}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create player: %d", resp.StatusCode)
	}

	if body := getBody(t, client, base+"/"); !strings.Contains(body, "Server is hibernating") {
		t.Fatal("admin page must show the journal tail")
	}

	pjar, _ := cookiejar.New(nil)
	pclient := &http.Client{CheckRedirect: client.CheckRedirect, Jar: pjar}
	loginAs(t, pclient, base, "carol", "carolpass1")
	if body := getBody(t, pclient, base+"/"); strings.Contains(body, "Server is hibernating") {
		t.Fatal("players must not see the server journal")
	}
}

// Refresh on the log card swaps the card in place. It used to be a GET on "/",
// which reloaded the page and threw away the flash that explained a failure.
func TestServerLogsPartialIsAdminOnly(t *testing.T) {
	client, _, base := newPanelTest(t)
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")
	if resp := postForm(t, client, base+"/users/create", url.Values{
		"username": {"dave"}, "password": {"davepass12"}, "role": {"player"},
	}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create player: %d", resp.StatusCode)
	}

	body := getBody(t, client, base+"/partials/server-logs")
	if !strings.Contains(body, "Server is hibernating") {
		t.Fatalf("partial did not render the journal:\n%s", body)
	}
	// It must be a fragment, not a whole page.
	if strings.Contains(body, "<html") {
		t.Fatal("the partial must not render the full layout")
	}
	// The page card must target the partial rather than reloading "/".
	if !strings.Contains(getBody(t, client, base+"/"), `hx-get="/partials/server-logs"`) {
		t.Fatal("the log card's Refresh must use the partial")
	}

	pjar, _ := cookiejar.New(nil)
	pclient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Jar: pjar}
	loginAs(t, pclient, base, "dave", "davepass12")
	resp := get(t, pclient, base+"/partials/server-logs")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("players must be redirected away from the journal, got %d", resp.StatusCode)
	}
}

// Installed state and config availability come from typed fields now. The panel
// used to parse "[installed v1.2] …" back out of the description text and
// advertised a config editor on every card.
func TestPluginCardsUseTypedState(t *testing.T) {
	client, fa, base := newPanelTest(t)
	fa.plugins = `{"plugins":[
		{"id":"weaponpaints","name":"WeaponPaints","description":"skins","kind":"plugin",
		 "config_path":"addons/counterstrikesharp/configs/plugins/WeaponPaints/WeaponPaints.json",
		 "installed":true,"installed_version":"v3.2 [beta]"},
		{"id":"metamod","name":"Metamod:Source","description":"loader","kind":"runtime"}]}`
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	body := getBody(t, client, base+"/plugins")
	// A version containing "]" used to break the annotation parser.
	if !strings.Contains(body, "v3.2 [beta]") {
		t.Fatalf("installed version not rendered:\n%s", body[:min(1500, len(body))])
	}
	if !strings.Contains(body, "/plugins/weaponpaints/config") {
		t.Fatal("a plugin with a config path must offer the editor")
	}
	if strings.Contains(body, "/plugins/metamod/config") {
		t.Fatal("metamod has no editable config and must not offer one")
	}
	if !strings.Contains(body, "not installed") {
		t.Fatal("metamod should render as not installed")
	}
}

// A failed install must read as a sentence, not as a chain of Go error
// prefixes. This is the exact message the user reported.
func TestHumanJobError(t *testing.T) {
	got := humanJobError("plugins: dependency cssharp: plugins: dependency metamod: " +
		"plugins: metamod: read pointer: unexpected EOF")
	if strings.HasPrefix(got, "plugins:") {
		t.Fatalf("internal prefix survived: %q", got)
	}
	if !strings.HasPrefix(got, "Dependency") {
		t.Fatalf("message = %q", got)
	}
	if humanJobError("") != "" {
		t.Fatal("empty message must stay empty")
	}
}

// A successful install with a warning must surface it: the plugin is installed
// but will not load, which is not something to hide behind a green badge.
func TestPluginJobWarningIsShown(t *testing.T) {
	client, _, base := newPanelTestWithJobs(t, `{"jobs":[{"id":"job1","kind":"install",
		"target":"weaponpaints","label":"WeaponPaints","status":"done",
		"result":{"id":"weaponpaints","version":"v3.2","requires_restart":true,
		"warning":"WeaponPaints will not load: gamedata/weaponpaints.json was not in the release archive"}}]}`)
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	body := getBody(t, client, base+"/plugins")
	if !strings.Contains(body, "gamedata/weaponpaints.json") {
		t.Fatalf("install warning not surfaced:\n%s", body[:min(1500, len(body))])
	}
	if !strings.Contains(body, "warning") {
		t.Fatal("a warning badge should distinguish it from a clean install")
	}
}

// Uninstalling a component something else depends on would break the server, so
// the card explains the dependency instead of offering the button.
func TestPluginCardHidesUninstallWhenDependedOn(t *testing.T) {
	client, fa, base := newPanelTest(t)
	fa.plugins = `{"plugins":[
		{"id":"metamod","name":"Metamod:Source","description":"loader","kind":"runtime",
		 "installed":true,"installed_version":"v2.0","required_by":["CounterStrikeSharp","CS2Fixes"]},
		{"id":"cssharp","name":"CounterStrikeSharp","description":"runtime","kind":"runtime",
		 "requires":["metamod"],"installed":true,"installed_version":"v1.0"}]}`
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	body := getBody(t, client, base+"/plugins")
	if strings.Contains(body, "/plugins/metamod/uninstall") {
		t.Fatal("metamod must not offer uninstall while it is depended on")
	}
	if !strings.Contains(body, "CounterStrikeSharp and CS2Fixes") {
		t.Fatalf("the card must name the dependents:\n%s", body[:min(2000, len(body))])
	}
	// The leaf component is still removable.
	if !strings.Contains(body, "/plugins/cssharp/uninstall") {
		t.Fatal("cssharp has no dependents and must be removable")
	}
}

// When the last install finishes, the polled strip must also refresh the cards
// out-of-band. Without it a freshly installed plugin kept saying "not installed"
// (and hid its Configure link) until the operator reloaded the page by hand.
func TestPluginJobsPartialRefreshesCardsWhenIdle(t *testing.T) {
	client, fa, base := newPanelTestWithJobs(t, `{"jobs":[{"id":"job1","kind":"install",
		"target":"weaponpaints","label":"WeaponPaints","status":"done",
		"result":{"id":"weaponpaints","version":"v3.2","requires_restart":true}}]}`)
	fa.plugins = `{"plugins":[{"id":"weaponpaints","name":"WeaponPaints","description":"skins",
		"kind":"plugin","installed":true,"installed_version":"v3.2",
		"config_path":"addons/counterstrikesharp/configs/plugins/WeaponPaints/WeaponPaints.json"}]}`
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	body := getBody(t, client, base+"/partials/plugin-jobs")
	if !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Fatalf("finished jobs must swap the card grid out-of-band:\n%s", body)
	}
	if !strings.Contains(body, "installed v3.2") {
		t.Fatalf("refreshed cards must show the new state:\n%s", body)
	}

	// While a job is still running the grid must NOT be swapped: the operator
	// may be scrolling it, and nothing has changed yet.
	client2, fa2, base2 := newPanelTestWithJobs(t, `{"jobs":[{"id":"job2","kind":"install",
		"target":"cssharp","label":"CounterStrikeSharp","status":"running","step":"downloading"}]}`)
	fa2.plugins = `{"plugins":[{"id":"cssharp","name":"CounterStrikeSharp","description":"runtime","kind":"runtime"}]}`
	_ = get(t, client2, base2+"/setup")
	_ = postForm(t, client2, base2+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client2, base2, "admin", "password123")
	if body := getBody(t, client2, base2+"/partials/plugin-jobs"); strings.Contains(body, `hx-swap-oob`) {
		t.Fatalf("cards must not be swapped while a job runs:\n%s", body)
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

// The status poll is the only thing that refreshes on its own, so everything
// that depends on "is the server running" has to travel with it. The lifecycle
// buttons and the player list used to sit outside it: after a crash the status
// card read "Server is stopped" next to a live Restart/Stop pair, and the player
// counts moved while the name list stayed frozen.
func TestStatusPollRefreshesActionsAndPlayers(t *testing.T) {
	client, fa, base := newPanelTest(t)
	fa.statusBody = `{
		"service": {"active": true, "enabled": true, "uptime_seconds": 90},
		"info": {"name": "cs2a test server", "map": "de_dust2", "players": 1, "max": 12},
		"rcon": {"hostname": "cs2a test server", "map": "de_dust2",
			"players": [{"user_id":"2","name":"Ana","steam_id":"76561198000000001","connected":"05:11","ping":24,"state":"active"}],
			"humans": 1, "bots": 0, "max": 12}
	}`
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	// A full page render must contain each region exactly once and must not
	// carry out-of-band markers (they would duplicate ids on first paint).
	page := getBody(t, client, base+"/")
	for _, id := range []string{`id="lifecycle-actions"`, `id="players-card"`, `id="status-card"`} {
		if n := strings.Count(page, id); n != 1 {
			t.Fatalf("page contains %s %d times, want 1", id, n)
		}
	}
	if strings.Contains(page, "hx-swap-oob") {
		t.Fatal("full page render must not emit out-of-band swaps")
	}

	// The poll response carries all three.
	partial := getBody(t, client, base+"/partials/status-card")
	if !strings.Contains(partial, `id="lifecycle-actions" hx-swap-oob="true"`) {
		t.Fatalf("poll must refresh the lifecycle buttons out-of-band:\n%s", partial)
	}
	if !strings.Contains(partial, `id="players-card" hx-swap-oob="true"`) {
		t.Fatalf("poll must refresh the player list out-of-band:\n%s", partial)
	}
	if !strings.Contains(partial, "Ana") {
		t.Fatalf("player list must carry the live roster:\n%s", partial)
	}
	// Running server: stop/restart, no start.
	if !strings.Contains(partial, "/do/restart") || strings.Contains(partial, "/do/start") {
		t.Fatalf("a running server must offer restart/stop only:\n%s", partial)
	}

	// Now the server is down: the same poll must flip the buttons.
	fa.mu.Lock()
	fa.statusBody = `{"service": {"active": false, "enabled": true}}`
	fa.mu.Unlock()
	partial = getBody(t, client, base+"/partials/status-card")
	if !strings.Contains(partial, "/do/start") {
		t.Fatalf("a stopped server must offer start:\n%s", partial)
	}
	if strings.Contains(partial, "/do/stop") {
		t.Fatalf("a stopped server must not offer stop:\n%s", partial)
	}
	if !strings.Contains(partial, "Nobody is connected right now") {
		t.Fatalf("the player list must empty out too:\n%s", partial)
	}
}

// A player has no lifecycle controls at all, so the poll must not send them the
// admin-only buttons (htmx would happily insert markup they cannot use, and the
// forms post to routes that reject them).
func TestStatusPollOmitsActionsForPlayers(t *testing.T) {
	client, fa, base := newPanelTest(t)
	fa.statusBody = `{"service":{"active":true},"info":{"name":"s","map":"de_dust2","players":0,"max":10}}`
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")
	if resp := postForm(t, client, base+"/users/create", url.Values{
		"username": {"pat"}, "password": {"patpass123"}, "role": {"player"},
	}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create player: %d", resp.StatusCode)
	}
	pc := newPlayerClient(t, client)
	loginAs(t, pc, base, "pat", "patpass123")

	partial := getBody(t, pc, base+"/partials/status-card")
	if strings.Contains(partial, "lifecycle-actions") {
		t.Fatalf("players must not receive lifecycle controls:\n%s", partial)
	}
	if !strings.Contains(partial, `id="players-card" hx-swap-oob="true"`) {
		t.Fatalf("players still get the refreshed roster:\n%s", partial)
	}
}

// Switching whitelist enforcement on is the one action that can lock the
// operator out of their own server. The agent refuses an empty list, so the
// switch must not offer the click at all; with entries it must say how many
// people will still be able to join.
func TestWhitelistEnableIsConfirmed(t *testing.T) {
	client, fa, base := newPanelTest(t)
	fa.wlEmpty = true
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	body := getBody(t, client, base+"/access")
	if !strings.Contains(body, `disabled aria-disabled="true"`) {
		t.Fatalf("an empty whitelist must not offer an enforce button that always fails:\n%s", body)
	}
	if strings.Contains(body, `name="enabled" value="1"`) {
		t.Fatal("the enable form must not be submittable while the list is empty")
	}

	// With entries the warning names the count.
	fa.mu.Lock()
	fa.wlEmpty = false
	fa.whitelist = []string{"76561198000000001", "76561198000000002"}
	fa.mu.Unlock()
	body = getBody(t, client, base+"/access")
	if !strings.Contains(body, "Enforce the whitelist? Only the 2 listed SteamIDs") {
		t.Fatalf("the confirmation must name how many IDs are allowed:\n%s", body)
	}
	if !strings.Contains(body, `name="enabled" value="1"`) {
		t.Fatal("a non-empty whitelist must be enforceable")
	}
}

// Every page's flash must be dismissible by the shared app.js handler, which
// looks the element up by id. Four pages had no id at all, so their success
// banners never cleared.
func TestEveryFlashCarriesTheDismissID(t *testing.T) {
	client, _, base := newPanelTest(t)
	_ = get(t, client, base+"/setup")
	_ = postForm(t, client, base+"/setup", url.Values{"token": {"setuptok"}, "username": {"admin"}, "password": {"password123"}})
	loginAs(t, client, base, "admin", "password123")

	for _, path := range []string{"/", "/plugins", "/access", "/users"} {
		body := getBody(t, client, base+path+"?ok=saved+it")
		if !strings.Contains(body, `id="flash"`) {
			t.Errorf("%s: flash has no id, so it can never auto-dismiss", path)
		}
		if !strings.Contains(body, "saved it") {
			t.Errorf("%s: flash text missing", path)
		}
		if n := strings.Count(body, `id="flash"`); n != 1 {
			t.Errorf("%s: %d flash elements, want 1", path, n)
		}
	}
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
