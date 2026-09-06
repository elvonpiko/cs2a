package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// newAuthClient builds a client that presents the agent token, for tests that
// wire their own API instance instead of using newTestAPI.
func newAuthClient(token string) *http.Client {
	return &http.Client{Transport: authTransport{base: http.DefaultTransport, token: token}}
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

// A lifecycle action must report the unit's real state, not just that
// systemctl exited 0.
func TestAPILifecycleAndStatus(t *testing.T) {
	client, svc, base, _ := newTestAPI(t)

	resp, out := doJSON(t, client, "POST", base, "/api/v1/server/stop", nil)
	if resp.StatusCode != 200 || out["failed"] == true || out["active"] != false {
		t.Fatalf("stop: %d %v", resp.StatusCode, out)
	}
	if svc.active {
		t.Fatal("service should be stopped")
	}
	resp, out = doJSON(t, client, "POST", base, "/api/v1/server/start", nil)
	if resp.StatusCode != 200 || out["failed"] == true || out["active"] != true || !svc.active {
		t.Fatalf("start: %d %v (active=%v)", resp.StatusCode, out, svc.active)
	}
	resp, out = doJSON(t, client, "GET", base, "/api/v1/status", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status code %d", resp.StatusCode)
	}
	if _, ok := out["service"]; !ok {
		t.Fatalf("status missing service: %v", out)
	}
}

// A unit that exits immediately after start must be reported as a failure —
// the panel showed a cheerful "starting…" for a server that was already dead.
func TestAPIStartReportsUnitThatDies(t *testing.T) {
	client, svc, base, _ := newTestAPI(t)
	svc.dieOnStart = true

	resp, out := doJSON(t, client, "POST", base, "/api/v1/server/start", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 for a unit that did not stay up, got %d: %v", resp.StatusCode, out)
	}
	if out["failed"] != true {
		t.Fatalf("failed flag missing: %v", out)
	}
	msg, _ := out["message"].(string)
	if !strings.Contains(msg, "did not stay running") {
		t.Fatalf("unhelpful message: %q", msg)
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
	// installed reports whether the feature exists at all; the access page
	// hides the whole card until the plugin is on the server.
	if _, ok := out["installed"].(bool); !ok {
		t.Fatalf("installed missing from whitelist state: %v", out)
	}
}

// Enforcing an empty whitelist rejects every connection, including the
// operator's, and the only way back would be editing files over SSH. The agent
// refuses it regardless of what the caller sends.
func TestAPIWhitelistRefusesEnforcingEmptyList(t *testing.T) {
	client, _, base, _ := newTestAPI(t)
	resp, out := doJSON(t, client, "PUT", base, "/api/v1/whitelist/enabled", map[string]any{"enabled": true})
	if resp.StatusCode != 400 {
		t.Fatalf("enforcing an empty whitelist: %d %v, want 400", resp.StatusCode, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "empty whitelist") {
		t.Fatalf("error should explain the refusal, got %q", msg)
	}
	// It must not have been switched on behind the refusal.
	_, out = doJSON(t, client, "GET", base, "/api/v1/whitelist", nil)
	if out["enabled"] != false {
		t.Fatalf("enabled = %v after a refused change", out["enabled"])
	}

	// With an entry the same call succeeds.
	if resp, out := doJSON(t, client, "PUT", base, "/api/v1/whitelist", map[string]any{
		"steamids": []string{"76561197961500295"},
	}); resp.StatusCode != 200 {
		t.Fatalf("put whitelist: %d %v", resp.StatusCode, out)
	}
	if resp, out := doJSON(t, client, "PUT", base, "/api/v1/whitelist/enabled", map[string]any{"enabled": true}); resp.StatusCode != 200 {
		t.Fatalf("enable with entries: %d %v", resp.StatusCode, out)
	}
	// Clearing the list while enforcement is on is allowed — it is the switch
	// that is guarded, not the file — but turning it off always works.
	if resp, out := doJSON(t, client, "PUT", base, "/api/v1/whitelist/enabled", map[string]any{"enabled": false}); resp.StatusCode != 200 {
		t.Fatalf("disable: %d %v", resp.StatusCode, out)
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

// The panel reads the journal through the agent so an operator can diagnose a
// server that will not start without SSH access.
func TestAPILogs(t *testing.T) {
	client, svc, base, _ := newTestAPI(t)
	svc.journal = []string{"cs2-server[42]: Server is hibernating", ""}

	resp, out := doJSON(t, client, "GET", base, "/api/v1/server/logs?n=5", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("logs: %d %v", resp.StatusCode, out)
	}
	lines, _ := out["lines"].([]any)
	if len(lines) != 1 || !strings.Contains(lines[0].(string), "hibernating") {
		t.Fatalf("lines = %v", out)
	}

	// n must be sanitised, not trusted.
	if resp, _ := doJSON(t, client, "GET", base, "/api/v1/server/logs?n=-3", nil); resp.StatusCode != 200 {
		t.Fatalf("negative n: %d", resp.StatusCode)
	}
	if resp, _ := doJSON(t, client, "GET", base, "/api/v1/server/logs?n=abc", nil); resp.StatusCode != 200 {
		t.Fatalf("non-numeric n: %d", resp.StatusCode)
	}
}

// rcon-check reports the diagnosis; rcon-repair applies it. Both are what
// replaced the bare "connection refused" the operator used to get.
func TestAPIRCONCheckAndRepair(t *testing.T) {
	client, _, base, _ := newTestAPI(t)

	resp, out := doJSON(t, client, "GET", base, "/api/v1/server/rcon-check", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("rcon-check: %d %v", resp.StatusCode, out)
	}
	if _, ok := out["ok"]; !ok {
		t.Fatalf("diagnosis has no ok field: %v", out)
	}
	// The fake unit reports no launch line, so nothing is repairable and the
	// endpoint must say so rather than half-applying something.
	resp, out = doJSON(t, client, "POST", base, "/api/v1/server/rcon-repair", nil)
	if resp.StatusCode != 200 && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("rcon-repair: %d %v", resp.StatusCode, out)
	}
	if resp.StatusCode == 200 {
		if _, ok := out["applied"]; !ok {
			t.Fatalf("repair response missing applied: %v", out)
		}
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

// A download in flight must publish its byte counters through the jobs API:
// the panel's progress bar is driven by download_bytes/download_total, and a
// stalled bar is indistinguishable from a broken one.
func TestAPIJobReportsDownloadProgress(t *testing.T) {
	cfg := testConfig(t)
	svc := &fakeService{active: true}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	// A "slow" upstream: it announces a 10-byte body, writes 4, then holds
	// the connection open until the test has seen the mid-flight counters.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		io.WriteString(w, "abcd")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
	}))
	// Cleanups run LIFO: the handlers must be released (release closed) BEFORE
	// slow.Close() waits for them to return, or the two deadlock.
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })

	srv := &Server{cfg: cfg, sysd: svc, store: store}
	wh := NewWhitelist(cfg)
	// A direct-URL entry: no GitHub resolution is needed, and the artifact
	// URL already points at the local slow server, so the default transport
	// (no rewriting) is exactly right.
	gh := NewGHClient("")
	gh.HTTP.Transport = http.DefaultTransport
	inst := NewInstaller(cfg, store, []CatalogEntry{{
		ID: "slowthing", Name: "Slow Thing", Kind: KindRuntime,
		URL: slow.URL + "/slowthing.zip",
	}}, gh)
	lo := NewLoadoutStore(cfg, store)
	t.Cleanup(lo.Close)
	api := NewAPI(cfg, srv, wh, inst, lo)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	client := &http.Client{Transport: authTransport{base: http.DefaultTransport, token: cfg.Token}}
	resp, out := doJSON(t, client, "POST", ts.URL, "/api/v1/plugins/slowthing/install", map[string]any{"async": true})
	if resp.StatusCode != 202 {
		t.Fatalf("async install: %d %v", resp.StatusCode, out)
	}
	id, _ := out["id"].(string)

	// Poll until the job publishes the mid-flight counters.
	var sawBytes bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, out = doJSON(t, client, "GET", ts.URL, "/api/v1/jobs/"+id, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("job status: %d", resp.StatusCode)
		}
		if b, _ := out["download_bytes"].(float64); b >= 4 {
			if tt, _ := out["download_total"].(float64); tt == 10 {
				sawBytes = true
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawBytes {
		t.Fatalf("job never reported mid-flight download bytes: %v", out)
	}
}

// A finished install whose restart hint is stale (the server booted again
// after the install finished) must say so, or the strip keeps telling the
// operator to do something they already did.
func TestAPIJobMarksRestarts(t *testing.T) {
	cfg := testConfig(t)
	svc := &fakeService{active: true}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv := &Server{cfg: cfg, sysd: svc, store: store}
	lo := NewLoadoutStore(cfg, store)
	t.Cleanup(lo.Close)
	api := NewAPI(cfg, srv, NewWhitelist(cfg), NewInstaller(cfg, store, DefaultCatalog(), nil), lo)
	// The default retention (10 min) would reap the finished-two-hours-ago
	// job before the list ever sees it.
	api.jobs.Retain = 3 * time.Hour
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	// The fake reports a fixed uptime of 3600s, so the current boot is
	// "now minus an hour": a job finished two hours ago predates the boot
	// (restart happened after it — hint stale), one finished now does not.
	old := Job{
		ID: "old", Kind: "install", Target: "x", Label: "X", Status: JobDone,
		Started: time.Now().Add(-2 * time.Hour), Finished: time.Now().Add(-2 * time.Hour),
		Result: &InstallResult{RequiresRestart: true},
	}
	fresh := Job{
		ID: "new", Kind: "install", Target: "y", Label: "Y", Status: JobDone,
		Started: time.Now(), Finished: time.Now(),
		Result: &InstallResult{RequiresRestart: true},
	}
	api.jobs.mu.Lock()
	api.jobs.jobs[old.ID] = &old
	api.jobs.jobs[fresh.ID] = &fresh
	api.jobs.mu.Unlock()

	client := &http.Client{Transport: authTransport{base: http.DefaultTransport, token: cfg.Token}}
	resp, out := doJSON(t, client, "GET", ts.URL, "/api/v1/jobs", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("jobs: %d", resp.StatusCode)
	}
	jobs, _ := out["jobs"].([]any)
	seen := map[string]bool{}
	for _, j := range jobs {
		m := j.(map[string]any)
		seen[m["id"].(string)] = m["restart_observed"] == true
	}
	if !seen["old"] {
		t.Fatal("an install followed by a server restart must be restart_observed")
	}
	if seen["new"] {
		t.Fatal("an install finished after the current boot must not be restart_observed")
	}
}

// Changing the map over the panel must also record it for the next server
// start: without the env write, a restart dragged the server back to the
// unit's hardcoded de_dust2 no matter what the operator had picked.
func TestAPIMapChangePersistsForNextStart(t *testing.T) {
	cfg := testConfig(t)
	svc := &fakeService{active: true}
	fake := startFakeRCON(t, "testpw", nil)
	cfg.RCONAddr = fake.addr()
	cfg.RCONPassword = "testpw"
	cfg.MapEnvFile = filepath.Join(t.TempDir(), "cs2a-map")
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv := &Server{cfg: cfg, sysd: svc, store: store}
	lo := NewLoadoutStore(cfg, store)
	t.Cleanup(lo.Close)
	api := NewAPI(cfg, srv, NewWhitelist(cfg), NewInstaller(cfg, store, DefaultCatalog(), nil), lo)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)

	mapsDir := filepath.Join(cfg.CSGODir(), "maps")
	if err := os.MkdirAll(mapsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"de_cache.vpk"} {
		if err := os.WriteFile(filepath.Join(mapsDir, m), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	client := &http.Client{Transport: authTransport{base: http.DefaultTransport, token: cfg.Token}}
	resp, out := doJSON(t, client, "POST", ts.URL, "/api/v1/map", map[string]any{"map": "de_cache"})
	if resp.StatusCode != 200 {
		t.Fatalf("map change: %d %v", resp.StatusCode, out)
	}
	if got := readMapEnv(cfg.MapEnvFile); got != "de_cache" {
		t.Fatalf("env file = %q, want de_cache — restart would lose the map", got)
	}

	// A workshop id cannot be replayed by +map, so it must not be recorded.
	resp, out = doJSON(t, client, "POST", ts.URL, "/api/v1/map", map[string]any{"map": "3234455566", "force": true})
	if resp.StatusCode != 200 {
		t.Fatalf("workshop map: %d %v", resp.StatusCode, out)
	}
	if got := readMapEnv(cfg.MapEnvFile); got != "de_cache" {
		t.Fatalf("workshop id overwrote the persisted map: %q", got)
	}
	if w, _ := out["warning"].(string); w == "" {
		t.Fatalf("workshop change must warn it will not survive a restart: %v", out)
	}
}
