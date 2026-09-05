package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The whitelist plugin reads core.cfg once, when Metamod finishes loading
// plugins, and copies "Enable" into its own mm_whitelist_enable cvar. Writing
// the file was therefore invisible to the running server: the Access page said
// the switch was flipped while the server kept enforcing the old value until
// somebody restarted it.
//
// The command names matter as much as the behaviour: cs2a used to send
// "wl_reload", which no released version of the plugin has ever implemented, so
// every "applied live" claim was a no-op the server silently ignored.
func TestWhitelistTogglePushesTheLiveCvar(t *testing.T) {
	cfg := testConfig(t)
	fake := startFakeRCON(t, "testpw", nil)
	cfg.RCONAddr = fake.addr()
	cfg.RCONPassword = "testpw"
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv := &Server{cfg: cfg, sysd: &fakeService{active: true}, store: store}
	wh := NewWhitelist(cfg)
	gh := NewGHClient("")
	gh.HTTP.Transport = offlineTransport{}
	inst := NewInstaller(cfg, store, DefaultCatalog(), gh)
	lo := NewLoadoutStore(cfg, store)
	t.Cleanup(lo.Close)
	api := NewAPI(cfg, srv, wh, inst, lo)
	srvTS := httptest.NewServer(api.Handler())
	t.Cleanup(srvTS.Close)
	ts := srvTS.URL
	client := newAuthClient(cfg.Token)

	// A list first: enforcing an empty one is refused for good reason.
	if resp, out := doJSON(t, client, "PUT", ts, "/api/v1/whitelist", map[string]any{
		"steamids": []string{"76561197961500295"},
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("put whitelist: %d %v", resp.StatusCode, out)
	}
	// Writing the list must also reload it in-game and drop the plugin's
	// per-map decision cache, or a player it already rejected stays rejected
	// until the map changes.
	sent := strings.Join(fake.sent(), "\n")
	if !strings.Contains(sent, "mm_whitelist_reload") {
		t.Errorf("saving the list did not reload it in-game; sent: %v", fake.sent())
	}
	if !strings.Contains(sent, "mm_whitelist_cache_clear") {
		t.Errorf("saving the list did not clear the decision cache; sent: %v", fake.sent())
	}
	if strings.Contains(sent, "wl_reload") {
		t.Errorf("wl_reload is not a command this plugin has; sent: %v", fake.sent())
	}

	if resp, out := doJSON(t, client, "PUT", ts, "/api/v1/whitelist/enabled",
		map[string]any{"enabled": true}); resp.StatusCode != http.StatusOK {
		t.Fatalf("enable: %d %v", resp.StatusCode, out)
	}
	sent = strings.Join(fake.sent(), "\n")
	if !strings.Contains(sent, "mm_whitelist_enable 1") {
		t.Errorf("enabling did not push the live cvar; sent: %v", fake.sent())
	}
	// The file is the persistent side of the same switch.
	raw, err := os.ReadFile(wh.CorePath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"Enable"`) || !strings.Contains(string(raw), `"1"`) {
		t.Fatalf("core.cfg does not record enforcement:\n%s", raw)
	}

	if resp, out := doJSON(t, client, "PUT", ts, "/api/v1/whitelist/enabled",
		map[string]any{"enabled": false}); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable: %d %v", resp.StatusCode, out)
	}
	if sent = strings.Join(fake.sent(), "\n"); !strings.Contains(sent, "mm_whitelist_enable 0") {
		t.Errorf("disabling did not push the live cvar; sent: %v", fake.sent())
	}
}

// A nil controller must not panic: the whitelist is usable without RCON, and
// the file remains the source of truth.
func TestWhitelistLivePushWithoutAServerIsANoop(t *testing.T) {
	wh := NewWhitelist(testConfig(t))
	wh.PushLive(nil, true)
	wh.ReloadLive(nil)
}
