package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// newTestAPI builds an API over fakes and returns a client + the underlying
// fake service for assertions.
func newTestAPI(t *testing.T) (*http.Client, *fakeService, string, Config) {
	t.Helper()
	cfg := testConfig(t)
	svc := &fakeService{active: true}
	fake := startFakeRCON(t, "testpw", nil)
	cfg.RCONAddr = fake.addr()
	cfg.RCONPassword = "testpw"
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv := &Server{cfg: cfg, sysd: svc, store: store}
	wh := NewWhitelist(cfg)
	// Give the installer a client that fails instantly: these tests must never
	// reach the real internet, and the async-install test only cares that the
	// request returns a job rather than blocking on a download.
	gh := NewGHClient("")
	gh.HTTP.Transport = offlineTransport{}
	inst := NewInstaller(cfg, store, DefaultCatalog(), gh)
	lo := NewLoadoutStore(cfg, store)
	t.Cleanup(lo.Close)
	api := NewAPI(cfg, srv, wh, inst, lo)

	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	client := &http.Client{Transport: authTransport{base: http.DefaultTransport, token: cfg.Token}}
	return client, svc, ts.URL, cfg
}

// offlineTransport refuses every outbound request.
type offlineTransport struct{}

func (offlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, errors.New("network disabled in tests")
}

type authTransport struct {
	base  http.RoundTripper
	token string
}

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+a.token)
	return a.base.RoundTrip(r)
}

func doJSON(t *testing.T, client *http.Client, method, base, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("%s %s: decode: %v", method, path, err)
	}
	return resp, out
}

func TestAPIUnauthorizedWithoutToken(t *testing.T) {
	cfg := testConfig(t)
	svc := &fakeService{}
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	srv := &Server{cfg: cfg, sysd: svc, store: store}
	lo := NewLoadoutStore(cfg, store)
	defer lo.Close()
	api := NewAPI(cfg, srv, NewWhitelist(cfg), NewInstaller(cfg, store, DefaultCatalog(), nil), lo)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	// health is public
	resp, err = http.Get(ts.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health want 200, got %d", resp.StatusCode)
	}
}

func TestAPILifecycleAndStatus(t *testing.T) {
	client, svc, base, _ := newTestAPI(t)

	_, out := doJSON(t, client, "POST", base, "/api/v1/server/stop", nil)
	if out["ok"] != true {
		t.Fatalf("stop: %v", out)
	}
	if svc.active {
		t.Fatal("service should be stopped")
	}
	_, out = doJSON(t, client, "POST", base, "/api/v1/server/start", nil)
	if out["ok"] != true || !svc.active {
		t.Fatalf("start: %v (active=%v)", out, svc.active)
	}
	resp, out := doJSON(t, client, "GET", base, "/api/v1/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status code %d", resp.StatusCode)
	}
	if _, ok := out["service"]; !ok {
		t.Fatalf("status missing service: %v", out)
	}
}

func TestAPISettingsAndMaps(t *testing.T) {
	client, _, base, cfg := newTestAPI(t)
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	// maps (empty dir -> empty list)
	resp, out := doJSON(t, client, "GET", base, "/api/v1/maps", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("maps: %d", resp.StatusCode)
	}

	// settings round trip
	_, out = doJSON(t, client, "PUT", base, "/api/v1/settings", map[string]any{
		"settings": []map[string]any{
			{"name": "mp_maxrounds", "value": "24"},
		},
	})
	if out["ok"] != true {
		t.Fatalf("put settings: %v", out)
	}
	resp, out = doJSON(t, client, "GET", base, "/api/v1/settings", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings: %d", resp.StatusCode)
	}
	setts, _ := out["settings"].([]any)
	if len(setts) != 1 {
		t.Fatalf("settings = %v", out)
	}

	// invalid cvar rejected
	resp, out = doJSON(t, client, "PUT", base, "/api/v1/settings", map[string]any{
		"settings": []map[string]any{{"name": "bad name;", "value": "1"}},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid cvar: %d %v", resp.StatusCode, out)
	}

	// password
	resp, out = doJSON(t, client, "PUT", base, "/api/v1/password", map[string]any{"password": "s3cret"})
	if resp.StatusCode != 200 || out["ok"] != true {
		t.Fatalf("password: %d %v", resp.StatusCode, out)
	}
}

func TestAPIWhitelistRoundTrip(t *testing.T) {
	client, _, base, _ := newTestAPI(t)
	resp, out := doJSON(t, client, "PUT", base, "/api/v1/whitelist", map[string]any{
		"steamids": []string{"[U:1:1234567]"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("put whitelist: %d %v", resp.StatusCode, out)
	}
	_, out = doJSON(t, client, "GET", base, "/api/v1/whitelist", nil)
	ids, _ := out["steamids"].([]any)
	if len(ids) != 1 || ids[0] != "76561197961500295" {
		t.Fatalf("whitelist = %v", out)
	}
	// enforcement is reported separately and starts off
	if out["enabled"] != false {
		t.Fatalf("enabled = %v, want false before the switch is flipped", out["enabled"])
	}

	resp, out = doJSON(t, client, "PUT", base, "/api/v1/whitelist/enabled", map[string]any{"enabled": true})
	if resp.StatusCode != 200 || out["enabled"] != true {
		t.Fatalf("enable whitelist: %d %v", resp.StatusCode, out)
	}
	_, out = doJSON(t, client, "GET", base, "/api/v1/whitelist", nil)
	if out["enabled"] != true {
		t.Fatalf("enabled not persisted: %v", out)
	}
}

func TestAPIPluginsList(t *testing.T) {
	client, _, base, _ := newTestAPI(t)
	resp, out := doJSON(t, client, "GET", base, "/api/v1/plugins", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("plugins: %d", resp.StatusCode)
	}
	list, _ := out["plugins"].([]any)
	if len(list) < 4 {
		t.Fatalf("plugins = %d entries", len(list))
	}
}

// An unknown plugin id must 404 rather than start a job, and an async install
// must answer immediately with a job the caller can poll.
func TestAPIPluginInstallAsyncJob(t *testing.T) {
	client, _, base, _ := newTestAPI(t)
	resp, _ := doJSON(t, client, "POST", base, "/api/v1/plugins/nope/install", map[string]any{"async": true})
	if resp.StatusCode != 404 {
		t.Fatalf("unknown plugin: %d", resp.StatusCode)
	}

	// metamod's download will fail (no network in tests) — what matters is
	// that the request returns at once with a job id and the job settles.
	resp, out := doJSON(t, client, "POST", base, "/api/v1/plugins/metamod/install", map[string]any{"async": true})
	if resp.StatusCode != 202 {
		t.Fatalf("async install: %d %v", resp.StatusCode, out)
	}
	id, _ := out["id"].(string)
	if id == "" || out["status"] != "running" {
		t.Fatalf("job = %v", out)
	}
	if out["target"] != "metamod" || out["label"] != "Metamod:Source" {
		t.Fatalf("job target/label = %v", out)
	}

	resp, out = doJSON(t, client, "GET", base, "/api/v1/jobs/"+id, nil)
	if resp.StatusCode != 200 || out["id"] != id {
		t.Fatalf("job status: %d %v", resp.StatusCode, out)
	}
	resp, out = doJSON(t, client, "GET", base, "/api/v1/jobs", nil)
	jobs, _ := out["jobs"].([]any)
	if resp.StatusCode != 200 || len(jobs) != 1 {
		t.Fatalf("job list: %d %v", resp.StatusCode, out)
	}
	if resp, _ := doJSON(t, client, "GET", base, "/api/v1/jobs/does-not-exist", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown job: %d", resp.StatusCode)
	}
}

func TestAPIExec(t *testing.T) {
	client, _, base, _ := newTestAPI(t)
	resp, out := doJSON(t, client, "POST", base, "/api/v1/server/exec", map[string]any{"command": "mp_warmuptime 5"})
	if resp.StatusCode != 200 {
		t.Fatalf("exec: %d %v", resp.StatusCode, out)
	}
}

func TestAPILoadoutRoundTrip(t *testing.T) {
	client, _, base, _ := newTestAPI(t)
	resp, out := doJSON(t, client, "PUT", base, "/api/v1/loadout/76561197961500295", map[string]any{
		"loadout": map[string]any{"knife_t": "weapon_knife_karambit", "knife_ct": "weapon_bayonet"},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("put loadout: %d %v", resp.StatusCode, out)
	}
	resp, out = doJSON(t, client, "GET", base, "/api/v1/loadout/76561197961500295", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get loadout: %d", resp.StatusCode)
	}
	lo, _ := out["loadout"].(map[string]any)
	if lo["knife_t"] != "weapon_knife_karambit" {
		t.Fatalf("loadout = %v", out)
	}
	if out["sync_enabled"] != false {
		t.Fatalf("sync should be disabled without wp_dsn")
	}
}

func TestCvarNameValidation(t *testing.T) {
	valid := []string{"sv_password", "mp_maxrounds", "hostname", "sv_cheats", "a.b_c"}
	invalid := []string{"", "1abc", "has space", "semi;colon", "quote\"x"}
	for _, v := range valid {
		if !reCvarName.MatchString(v) {
			t.Errorf("%q should be valid", v)
		}
	}
	for _, v := range invalid {
		if reCvarName.MatchString(v) {
			t.Errorf("%q should be invalid", v)
		}
	}
}
