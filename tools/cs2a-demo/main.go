// cs2a-demo boots the real panel against a fake agent that serves rich,
// honest demo state, for capturing screenshots of the actual UI.
//
// The panel code is the production panel; only the agent is replaced by
// canned responses describing a plausible small server: running, three
// humans and a bot online, the recommended stack installed, one CS2
// update pending. The screenshots generated from this are the real
// interface — not mockups — and can be regenerated with one command
// whenever the panel changes.
//
// Usage:
//
//	go run ./tools/cs2a-demo [-addr 127.0.0.1:8800]
//
// Then open http://127.0.0.1:8800 and sign in with admin / demo-password.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"cs2a/internal/agent"
	"cs2a/internal/panel"
)

// demoUsers are the panel accounts seeded into the panel's own store: an
// admin with a linked SteamID (so the loadout tab shows), two linked
// players (so the whitelist resolves names), and one unlinked player (so
// the Users page shows both states honestly).
var demoUsers = []struct{ username, password, role, steamid string }{
	{"admin", "demo-password", "admin", "76561197961500295"},
	{"alice", "demo-password", "player", "76561198000000001"},
	{"bob", "demo-password", "player", "76561198000000002"},
	{"carol", "demo-password", "player", ""},
}

// Canned agent responses. Shapes mirror the real handlers byte-for-byte
// (the same structs the panel's AgentClient decodes).

const statusBody = `{
	"service": {"active": true, "enabled": true, "sub": "running", "uptime_seconds": 342512},
	"connect_addr": "31.171.101.123:27015",
	"info": {"name": "Dust only | cs2a", "map": "de_dust2", "players": 4, "max": 12, "bots": 1},
	"rcon": {"hostname": "Dust only | cs2a", "map": "de_dust2", "humans": 3, "bots": 1, "max": 12,
		"players": [
			{"user_id": "2", "name": "alice", "steam_id": "76561198000000001", "connected": "42:17", "ping": 21, "state": "active"},
			{"user_id": "3", "name": "bob", "steam_id": "76561198000000002", "connected": "12:05", "ping": 34, "state": "active"},
			{"user_id": "4", "name": "Nova", "steam_id": "76561198123456789", "connected": "03:44", "ping": 55, "state": "active"},
			{"user_id": "5", "name": "BOT Kev", "steam_id": "", "connected": "01:00:00", "ping": 0, "state": "active", "bot": true}
		]}
}`

const mapsBody = `{"maps":["de_dust2","de_mirage","de_inferno","de_nuke","de_overpass","de_ancient","de_anubis","de_train"]}`

const logsBody = `{"lines":[
	"Sep 10 18:02:11 vps cs2-server[1642]: Server is hibernating",
	"Sep 10 18:14:03 vps cs2-server[1642]: Server logging data to file logs/L090_001_18_14_03.log",
	"Sep 10 18:14:05 vps cs2-server[1642]: Connection to Steam servers successful.",
	"Sep 10 18:14:09 vps cs2-server[1642]: Server logging enabled.",
	"Sep 10 18:21:47 vps cs2-server[1642]: Client \"alice\" connected (31.171.101.123:27005)",
	"Sep 10 18:23:12 vps cs2-server[1642]: Client \"bob\" connected (31.171.100.7:27017)",
	"Sep 10 18:45:01 vps cs2-server[1642]: Client \"Nova\" connected (85.195.52.3:27113)"
]}`

const updateBody = `{
	"installed_build": "10422026",
	"latest_build": "10425184",
	"available": true,
	"available_since": "2026-09-10T09:12:00Z",
	"pending_players": true,
	"updating": false,
	"auto_update": true,
	"last_check": "2026-09-10T18:45:00Z"
}`

const pluginsBody = `{"plugins":[
	{"id":"metamod","name":"Metamod:Source","description":"Loads native and managed plugins into the engine at boot.","author":"AlliedModders","kind":"runtime","recommended":true,"installed":true,"installed_version":"2.0 x64"},
	{"id":"cssharp","name":"CounterStrikeSharp","description":"Runtime for C# plugins, hot-refreshable without a restart.","author":"roflmuffin","kind":"runtime","requires":["metamod"],"recommended":true,"installed":true,"installed_version":"v326"},
	{"id":"weaponpaints","name":"WeaponPaints","description":"Per-player skins, knives, gloves and agents, synced like the official loadout.","author":"daffyy","kind":"plugin","requires":["cssharp"],"recommended":true,"installed":true,"installed_version":"build-459","config_path":"addons/counterstrikesharp/configs/plugins/WeaponPaints.json"},
	{"id":"cs2whitelist","name":"CS2 Whitelist","description":"Restrictive access: the server refuses everyone except the SteamIDs on the list.","author":"Pairman","kind":"plugin","requires":["cssharp"],"recommended":true,"installed":true,"installed_version":"1.2.1","config_path":"addons/counterstrikesharp/configs/plugins/WhitelistConfig.json"},
	{"id":"matchzy","name":"MatchZy","description":"Practice, scrims and matches with vetoes, coaches and GOTV.","author":"sweaty","kind":"plugin","requires":["cssharp"]},
	{"id":"retakes","name":"Retakes","description":"Retake practice: bomb planted, players spawn to take or defend the site.","author":"B3none","kind":"plugin","requires":["cssharp"]}
]}`

const whitelistBody = `{"steamids":["76561198000000001","76561198000000002","76561198123456789"],"enabled":true,"installed":true}`

const jobsBody = `{"jobs":[
	{"id":"job-3f","kind":"install","target":"weaponpaints","label":"WeaponPaints","status":"done","step":"installed","version":"build-459","finished":true}
]}`

const settingsBody = `{"settings":[
	{"name":"sv_password","value":"scrims-only","comment":"required to join"},
	{"name":"sv_cheats","value":"0"}
]}`

// weaponPaintsConfig is the JSON config editor's document for WeaponPaints.
const weaponPaintsConfig = `{"exists":true,"json":{
	"DatabaseHost": "127.0.0.1",
	"DatabasePort": 3306,
	"DatabaseUser": "cs2a",
	"DatabaseName": "cs2_wp",
	"KnifeEnabled": true,
	"GlovesEnabled": true,
	"AgentsEnabled": true,
	"MusicKitEnabled": true,
	"CommandsSettings": {"!knife": true, "!ws": true, "!agents": true}
}}`

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// fakeAgent answers the agent's API with the canned demo state.
type fakeAgent struct {
	token     string
	cosmetics []byte
	mu        sync.Mutex
	// pluginConfig holds per-plugin config documents keyed by catalog id.
	pluginConfig map[string]string
}

func (f *fakeAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	p := r.URL.Path
	switch {
	case p == "/api/v1/health":
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case p == "/api/v1/status":
		writeJSON(w, http.StatusOK, json.RawMessage(statusBody))
	case p == "/api/v1/maps":
		writeJSON(w, http.StatusOK, json.RawMessage(mapsBody))
	case p == "/api/v1/server/logs":
		writeJSON(w, http.StatusOK, json.RawMessage(logsBody))
	case p == "/api/v1/server/update":
		writeJSON(w, http.StatusOK, json.RawMessage(updateBody))
	case p == "/api/v1/plugins":
		writeJSON(w, http.StatusOK, json.RawMessage(pluginsBody))
	case p == "/api/v1/plugins/weaponpaints/config" && r.Method == "GET":
		w.Write([]byte(weaponPaintsConfig))
	case p == "/api/v1/whitelist":
		writeJSON(w, http.StatusOK, json.RawMessage(whitelistBody))
	case p == "/api/v1/jobs":
		writeJSON(w, http.StatusOK, json.RawMessage(jobsBody))
	case p == "/api/v1/settings":
		writeJSON(w, http.StatusOK, json.RawMessage(settingsBody))
	case p == "/api/v1/cosmetics":
		w.Write(f.cosmetics)
	case p == "/api/v1/loadout/76561197961500295":
		writeJSON(w, http.StatusOK, map[string]any{
			"steamid": "76561197961500295",
			"knife_t": "weapon_knife_butterfly", "knife_ct": "weapon_knife_butterfly",
			"musickit": 70,
			"gloves":   map[string]int{"defindex": 5031, "paint": 10037},
			"agent_t":  "tm_leet_variantf", "agent_ct": "ctm_sas_variantf",
			"skins_t":  map[string]string{"7": "421", "9": "344"},
			"skins_ct": map[string]string{"60": "658"},
		})
	case strings.HasPrefix(p, "/api/v1/loadout/"):
		id := strings.TrimPrefix(p, "/api/v1/loadout/")
		writeJSON(w, http.StatusOK, map[string]any{"steamid": id})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found: " + p})
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8800", "listen address for the demo panel")
	flag.Parse()

	// A temp panel.db keeps the demo self-contained: seeding users into a
	// fresh store is deterministic, and the file vanishes with the process.
	tmp, err := os.MkdirTemp("", "cs2a-demo-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	dbPath := filepath.Join(tmp, "panel.db")

	store, err := panel.OpenStore(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	for _, u := range demoUsers {
		hash, err := panel.HashPassword(u.password)
		if err != nil {
			log.Fatal(err)
		}
		if _, err := store.CreateUser(u.username, hash, u.role, u.steamid); err != nil {
			log.Fatal(err)
		}
	}
	// A believable audit trail for the Users page.
	store.Audit("admin", "install", "installed WeaponPaints (build-459)")
	store.Audit("admin", "password", "server password set (sv_password)")
	store.Audit("admin", "whitelist enabled", "3 players allowed")

	fa := &fakeAgent{
		token:        "demo-agent-token",
		pluginConfig: map[string]string{},
	}
	// The cosmetics catalog is the real one — the same embedded game data
	// the production agent serves, so the loadout picker renders with the
	// actual skin images.
	tAgents, ctAgents := agent.Agents()
	cosm, err := json.Marshal(map[string]any{
		"gloves":       agent.Gloves(),
		"agents_t":     tAgents,
		"agents_ct":    ctAgents,
		"weapons":      agent.Weapons(),
		"sync_enabled": true,
	})
	if err != nil {
		log.Fatal(err)
	}
	fa.cosmetics = cosm

	agentTS := httptest.NewServer(fa)
	defer agentTS.Close()

	cfg := panel.DefaultConfig()
	cfg.AgentURL = agentTS.URL
	cfg.AgentToken = fa.token
	cfg.Listen = *addr
	cfg.DBPath = dbPath
	// No setup token: the demo ships with accounts, so the panel skips
	// first-run setup and goes straight to the login page.
	cfg.SetupTokenFile = filepath.Join(tmp, "none")

	srv := panel.NewServer(cfg, store, panel.NewAgentClient(cfg.AgentURL, cfg.AgentToken), demoLogger())

	fmt.Printf("cs2a-demo on http://%s  — sign in with admin / demo-password\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
}

func demoLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}
