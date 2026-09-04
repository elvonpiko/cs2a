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
	whitelist  []string
	loadout    map[string][2]string
	plugins    string
}

func (f *fakeAgent) handler() http.Handler {
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
		w.Write([]byte(`{"steamids":["76561197960287930"]}`))
	})
	mux.HandleFunc("GET /api/v1/plugins", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.Write([]byte(f.plugins))
	})
	mux.HandleFunc("POST /api/v1/plugins/weaponpaints/install", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.Write([]byte(`{"id":"weaponpaints","version":"v1.5.4","requires_restart":true,"installed_deps":true}`))
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
		w.Write([]byte(`{"settings":[{"name":"sv_password","value":"hunter2"},{"name":"mm_whitelist_enable","value":"1"}]}`))
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
	agentTS := httptest.NewServer(fa.handler())
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

	// player CAN see server page
	resp2, err := pclient.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp2)
	if !strings.Contains(body, "de_dust2") {
		t.Fatalf("player server page missing status")
	}
	if strings.Contains(body, "/do/restart") {
		t.Fatalf("player server page must not expose admin actions")
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
	client, _, base := newPanelTest(t)
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

	// install
	resp = postForm(t, client, base+"/plugins/weaponpaints/install", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("install: %d", resp.StatusCode)
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
