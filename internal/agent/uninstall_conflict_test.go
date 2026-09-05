package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// blockingTransport holds every request until release is closed, so a job stays
// observably "running" for as long as the test needs. The offline transport used
// elsewhere fails instantly, which makes the conflict window unobservable.
type blockingTransport struct {
	release chan struct{}
	started chan struct{}
	once    bool
}

func (b *blockingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !b.once {
		b.once = true
		close(b.started)
	}
	select {
	case <-b.release:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case <-time.After(30 * time.Second):
	}
	return nil, errors.New("network disabled in tests")
}

// newBlockingAPI is newTestAPI with an installer whose downloads block, so an
// async install job can be caught mid-flight.
func newBlockingAPI(t *testing.T) (*http.Client, string, *blockingTransport) {
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

	tr := &blockingTransport{release: make(chan struct{}), started: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-tr.release:
		default:
			close(tr.release)
		}
	})

	srv := &Server{cfg: cfg, sysd: svc, store: store}
	gh := NewGHClient("")
	gh.HTTP.Transport = tr
	inst := NewInstaller(cfg, store, DefaultCatalog(), gh)
	lo := NewLoadoutStore(cfg, store)
	t.Cleanup(lo.Close)
	api := NewAPI(cfg, srv, NewWhitelist(cfg), inst, lo)

	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	client := &http.Client{Transport: authTransport{base: http.DefaultTransport, token: cfg.Token}}
	return client, ts.URL, tr
}

// Uninstalling a plugin while its own install is still running deletes files the
// extraction is writing: the plugin ends up half-present with a recorded manifest
// that no longer describes the disk. The install is asynchronous, so the check
// has to consult the job registry — the dependency check cannot catch it, because
// the dependency is not installed yet.
func TestAPIUninstallDuringInstallIsRejected(t *testing.T) {
	client, base, tr := newBlockingAPI(t)

	resp, out := doJSON(t, client, "POST", base, "/api/v1/plugins/metamod/install", map[string]any{"async": true})
	if resp.StatusCode != 202 {
		t.Fatalf("async install: %d %v", resp.StatusCode, out)
	}
	if out["status"] != "running" {
		t.Fatalf("job = %v", out)
	}
	<-tr.started // the download is in flight and cannot finish yet

	resp, out = doJSON(t, client, "DELETE", base, "/api/v1/plugins/metamod", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("uninstall during install: %d %v, want 409", resp.StatusCode, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "being installed") {
		t.Errorf("the error must say why it was refused: %q", msg)
	}
	if !strings.Contains(msg, "Metamod") {
		t.Errorf("the error must name the plugin, not its catalog id: %q", msg)
	}
}

// The same protection has to cover dependencies: removing Metamod while
// CounterStrikeSharp installs breaks the install in flight, and CSSharp requires
// Metamod rather than being it.
func TestAPIUninstallDependencyDuringInstallIsRejected(t *testing.T) {
	client, base, tr := newBlockingAPI(t)

	resp, out := doJSON(t, client, "POST", base, "/api/v1/plugins/cssharp/install", map[string]any{"async": true})
	if resp.StatusCode != 202 {
		t.Fatalf("async install: %d %v", resp.StatusCode, out)
	}
	<-tr.started

	resp, out = doJSON(t, client, "DELETE", base, "/api/v1/plugins/metamod", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("uninstall of a dependency during install: %d %v, want 409", resp.StatusCode, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "needs") {
		t.Errorf("the error must explain the dependency: %q", msg)
	}
	if !strings.Contains(msg, "Metamod") {
		t.Errorf("the error must name the plugin being removed: %q", msg)
	}
}

// An unrelated plugin is not blocked by someone else's install.
func TestAPIUninstallUnrelatedPluginDuringInstall(t *testing.T) {
	client, base, tr := newBlockingAPI(t)

	if resp, out := doJSON(t, client, "POST", base, "/api/v1/plugins/metamod/install", map[string]any{"async": true}); resp.StatusCode != 202 {
		t.Fatalf("async install: %d %v", resp.StatusCode, out)
	}
	<-tr.started

	// weaponpaints is not installed, so the honest answer is 404 — the point is
	// that it is not refused with a conflict about metamod.
	resp, out := doJSON(t, client, "DELETE", base, "/api/v1/plugins/weaponpaints", nil)
	if resp.StatusCode == http.StatusConflict {
		t.Fatalf("an unrelated uninstall was blocked: %v", out)
	}
}
