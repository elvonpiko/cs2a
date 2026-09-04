package agent

import (
	"bytes"
	"encoding/json"
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
	inst := NewInstaller(cfg, store, DefaultCatalog(), nil)
	api := NewAPI(cfg, srv, wh, inst)

	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	client := &http.Client{Transport: authTransport{base: http.DefaultTransport, token: cfg.Token}}
	return client, svc, ts.URL, cfg
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
	api := NewAPI(cfg, srv, NewWhitelist(cfg), NewInstaller(cfg, store, DefaultCatalog(), nil))
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

func TestAPIExec(t *testing.T) {
	client, _, base, _ := newTestAPI(t)
	resp, out := doJSON(t, client, "POST", base, "/api/v1/server/exec", map[string]any{"command": "mp_warmuptime 5"})
	if resp.StatusCode != 200 {
		t.Fatalf("exec: %d %v", resp.StatusCode, out)
	}
}

func TestCvarNameValidation(t *testing.T) {
	valid := []string{"sv_password", "mp_maxrounds", "hostname", "mm_whitelist_enable", "a.b_c"}
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
