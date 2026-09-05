package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPasswordHashing(t *testing.T) {
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse battery staple") {
		t.Fatal("correct password rejected")
	}
	if CheckPassword(h, "wrong") {
		t.Fatal("wrong password accepted")
	}
}

func TestTokenGeneration(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if len(a) != 64 || a == b {
		t.Fatalf("bad tokens: %q %q", a, b)
	}
	if HashToken(a) == HashToken(b) {
		t.Fatal("token hashes collide")
	}
	if HashToken(a) != HashToken(a) {
		t.Fatal("token hash not deterministic")
	}
	if !ConstantTimeEqual("abc", "abc") || ConstantTimeEqual("abc", "abd") {
		t.Fatal("constant time compare broken")
	}
}

func TestAgentClientPaths(t *testing.T) {
	var got struct {
		method, path, auth string
		body               map[string]any
	}
	mux := http.NewServeMux()
	record := func(method, path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got.method = r.Method
			got.path = r.URL.Path
			got.auth = r.Header.Get("Authorization")
			if r.Body != nil {
				_ = json.NewDecoder(r.Body).Decode(&got.body)
			}
			w.Header().Set("Content-Type", "application/json")
			switch path {
			case "/api/v1/status":
				w.Write([]byte(`{"service":{"active":true,"enabled":true},"info":{"name":"n","map":"de_dust2","players":1,"max":12}}`))
			case "/api/v1/maps":
				w.Write([]byte(`{"maps":["de_dust2","de_mirage"]}`))
			case "/api/v1/server/exec":
				w.Write([]byte(`{"ok":true,"output":"hello"}`))
			case "/api/v1/server/restart":
				w.Write([]byte(`{"action":"restart","active":true,"sub":"running","message":"Server is running."}`))
			case "/api/v1/plugins":
				w.Write([]byte(`{"plugins":[{"id":"metamod","name":"Metamod:Source","kind":"runtime"}]}`))
			case "/api/v1/plugins/weaponpaints/install":
				w.Write([]byte(`{"id":"weaponpaints","version":"v1.5.4","requires_restart":true,"installed_deps":true}`))
			case "/api/v1/whitelist":
				if r.Method == http.MethodGet {
					w.Write([]byte(`{"steamids":["76561197961500295"]}`))
				} else {
					w.Write([]byte(`{"steamids":[]}`))
				}
			case "/api/v1/cosmetics":
				w.Write([]byte(`{"gloves":[{"defindex":5032,"paint":10010,"name":"Hand Wraps","image":"/static/img/gloves/x.png"}],"agents_t":[{"model":"tm_leet_variantf","name":"Elite Crew","team":2}],"agents_ct":[{"model":"ctm_st6_variantj","name":"SEAL","team":3}]}`))
			case "/api/v1/loadout/76561197961500295":
				w.Write([]byte(`{"loadout":{"knife_t":"weapon_knife_karambit","knife_ct":"weapon_bayonet","gloves_t":"5032:10010","gloves_ct":"5031:10008","agent_t":"tm_leet_variantf","agent_ct":"ctm_st6_variantj"},"sync_enabled":true}`))
			case "/api/v1/broken":
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"boom"}`))
			default:
				w.Write([]byte(`{"ok":true}`))
			}
		}
	}
	for _, p := range []string{"/api/v1/status", "/api/v1/maps", "/api/v1/server/exec",
		"/api/v1/plugins", "/api/v1/plugins/weaponpaints/install",
		"/api/v1/whitelist", "/api/v1/loadout/76561197961500295", "/api/v1/broken",
		"/api/v1/server/start", "/api/v1/server/stop", "/api/v1/server/restart",
		"/api/v1/settings", "/api/v1/password", "/api/v1/map", "/api/v1/plugins/weaponpaints"} {
		mux.HandleFunc(p, record(strings.Split(p, "/")[3], p))
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewAgentClient(ts.URL, "tok")
	ctx := context.Background()

	// status
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Service.Active || st.Info.Map != "de_dust2" {
		t.Fatalf("status = %+v", st)
	}

	// maps
	maps, err := c.Maps(ctx)
	if err != nil || len(maps) != 2 {
		t.Fatalf("maps = %v %v", maps, err)
	}

	// actions + exec: a lifecycle action reports the verified unit state
	res, err := c.ServerAction(ctx, "restart")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Active || res.Failed || res.Message == "" {
		t.Fatalf("restart = %+v", res)
	}
	out, err := c.Exec(ctx, "mp_warmuptime 5")
	if err != nil || out != "hello" {
		t.Fatalf("exec = %q %v", out, err)
	}

	// plugins + install
	plugins, err := c.Plugins(ctx)
	if err != nil || len(plugins) != 1 || plugins[0].Name != "Metamod:Source" {
		t.Fatalf("plugins = %+v %v", plugins, err)
	}
	ires, err := c.Install(ctx, "weaponpaints", false)
	if err != nil || !ires.RequiresRestart || ires.Version != "v1.5.4" {
		t.Fatalf("install = %+v %v", ires, err)
	}

	// whitelist round trip
	if err := c.PutWhitelist(ctx, []string{"76561197961500295"}); err != nil {
		t.Fatal(err)
	}
	ids, err := c.Whitelist(ctx)
	if err != nil || len(ids) != 1 {
		t.Fatalf("whitelist = %v %v", ids, err)
	}

	// loadout round trip (all six cosmetic fields)
	if _, _, err := c.PutLoadout(ctx, "76561197961500295", &PlayerLoadout{
		KnifeT: "weapon_knife_karambit", KnifeCT: "weapon_bayonet",
		GlovesT: "5032:10010", GlovesCT: "5031:10008",
		AgentT: "tm_leet_variantf", AgentCT: "ctm_st6_variantj",
	}); err != nil {
		t.Fatal(err)
	}
	lo, err := c.GetLoadout(ctx, "76561197961500295")
	if err != nil || lo.KnifeT != "weapon_knife_karambit" || lo.KnifeCT != "weapon_bayonet" ||
		lo.GlovesT != "5032:10010" || lo.AgentCT != "ctm_st6_variantj" {
		t.Fatalf("loadout = %+v %v", lo, err)
	}

	// password / settings / changemap
	if err := c.SetPassword(ctx, "pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PutSettings(ctx, []Setting{{Name: "mp_maxrounds", Value: "24"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.ChangeMap(ctx, "de_mirage", false); err != nil {
		t.Fatal(err)
	}

	// error propagation
	err = c.do(ctx, http.MethodGet, "/api/v1/broken", nil, nil)
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Status != 400 || apiErr.Message != "boom" {
		t.Fatalf("want APIError{400 boom}, got %#v", err)
	}

	// auth header present
	if got.auth != "Bearer tok" {
		t.Fatalf("auth header = %q", got.auth)
	}
}
