package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH serves GitHub-API-like release metadata and a downloadable asset.
func fakeGH(t *testing.T) (*httptest.Server, *GHClient) {
	t.Helper()
	base := ""
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/roflmuffin/CounterStrikeSharp/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GHRelease{
			TagName: "v100.0.0",
			Assets: []GHAsset{
				{Name: "CounterStrikeSharp-Site-Windows.zip", URL: base + "/assets/cssharp-site.zip"},
				{Name: "CounterStrikeSharp-with-runtime-linux-x64.zip", URL: base + "/assets/cssharp-runtime.zip"},
			},
		})
	})
	mux.HandleFunc("/repos/Nereziel/cs2-WeaponPaints/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GHRelease{
			TagName: "v1.5.4",
			Assets: []GHAsset{
				{Name: "WeaponPaints-1.5.4.zip", URL: base + "/assets/wp.zip"},
			},
		})
	})
	mux.HandleFunc("/mmsdrop/2.0/mmsource-latest-linux.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		body := "ELF"
		_ = tw.WriteHeader(&tar.Header{Name: "addons/metamod/metamod.2.cs2.so", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
		tw.Close()
		gw.Close()
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(buf.Bytes())
	})
	mux.HandleFunc("/assets/cssharp-runtime.zip", func(w http.ResponseWriter, r *http.Request) {
		zipBytes, _ := makeZip(map[string][]byte{
			"addons/counterstrikesharp/api/CSSharp.dll": {1},
			"addons/counterstrikesharp/dotnet/dotnet":   {2},
		})
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipBytes)
	})
	mux.HandleFunc("/assets/wp.zip", func(w http.ResponseWriter, r *http.Request) {
		zipBytes, _ := makeZip(map[string][]byte{
			"addons/counterstrikesharp/plugins/WeaponPaints/WeaponPaints.dll": {9},
		})
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipBytes)
	})
	srv := httptest.NewServer(mux)
	base = srv.URL // handlers capture by reference
	gh := NewGHClient("")
	// point the client at the fake server by rewriting api URLs
	gh.HTTP.Transport = rewriteTransport{base: http.DefaultTransport, to: srv.URL}
	t.Cleanup(srv.Close)
	return srv, gh
}

type rewriteTransport struct {
	base http.RoundTripper
	to   string
}

func (rw rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Host, "api.github.com") || strings.Contains(r.URL.Host, "mms.alliedmods.net") {
		r.URL.Host = strings.TrimPrefix(rw.to, "http://")
		r.URL.Scheme = "http"
	}
	return rw.base.RoundTrip(r)
}

func testConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Token:       "test",
		CS2Dir:      filepath.Join(dir, "cs2"),
		RCONAddr:    "127.0.0.1:1",
		DBPath:      filepath.Join(dir, "agent.db"),
		PluginCache: filepath.Join(dir, "cache"),
	}
}

func TestInstallerInstallsDepsAndRecordsState(t *testing.T) {
	_, gh := fakeGH(t)
	cfg := testConfig(t)
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), gh)

	// cssharp->metamod post-install patches gameinfo.gi; it must exist
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CSGODir(), "gameinfo.gi"), []byte("Game\tcsgo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := in.Install(context.Background(), "weaponpaints", false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.InstalledDeps {
		t.Fatal("expected cssharp+metamod to be installed as deps")
	}
	if res.Version != "v1.5.4" {
		t.Fatalf("version = %q", res.Version)
	}

	// files extracted into csgo dir
	dll := filepath.Join(cfg.CSGODir(), "addons/counterstrikesharp/plugins/WeaponPaints/WeaponPaints.dll")
	if !fileExists(dll) {
		t.Fatal("plugin dll missing")
	}
	// guidelines patch applied (weaponpaints post-install)
	core := filepath.Join(cfg.CSGODir(), "addons/counterstrikesharp/configs/core.json")
	raw, _ := os.ReadFile(core)
	if !bytes.Contains(raw, []byte(`"FollowCS2ServerGuidelines": false`)) {
		t.Fatalf("guidelines patch missing:\n%s", raw)
	}

	// state recorded for all three
	for _, id := range []string{"metamod", "cssharp", "weaponpaints"} {
		if !in.IsInstalled(id) {
			t.Errorf("%s not recorded as installed", id)
		}
	}
	st, _ := store.GetPluginState("weaponpaints")
	if st.Manifest["top0"] != "addons" {
		t.Fatalf("manifest = %+v", st.Manifest)
	}

	// second install is a no-op with the recorded version
	res2, err := in.Install(context.Background(), "weaponpaints", false)
	if err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if res2.InstalledDeps {
		t.Fatal("deps should already be installed")
	}
	if res2.Version != "v1.5.4" {
		t.Fatalf("reinstall version = %q", res2.Version)
	}
}

func TestInstallerMetamodPostInstallPatchesGameinfo(t *testing.T) {
	_, gh := fakeGH(t)
	cfg := testConfig(t)
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), gh)

	// gameinfo.gi must exist before install so the post-install step can
	// patch it
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(cfg.CSGODir(), "gameinfo.gi")
	if err := os.WriteFile(gi, []byte("\t\tGame\t\t\tcsgo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := in.Install(context.Background(), "metamod", false)
	if err != nil {
		t.Fatalf("install metamod: %v", err)
	}
	if res.Version != "latest" || res.RequiresRestart != true {
		t.Fatalf("res = %+v", res)
	}
	raw, _ := os.ReadFile(gi)
	if !bytes.Contains(raw, []byte("csgo/addons/metamod")) {
		t.Fatalf("gameinfo not patched:\n%s", raw)
	}
	so := filepath.Join(cfg.CSGODir(), "addons/metamod/metamod.2.cs2.so")
	if !fileExists(so) {
		t.Fatal("metamod file missing")
	}
}

func TestInstallerUnknownEntry(t *testing.T) {
	_, gh := fakeGH(t)
	cfg := testConfig(t)
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), gh)
	if _, err := in.Install(context.Background(), "nope", false); err == nil {
		t.Fatal("expected unknown-entry error")
	}
}

func TestUninstallRemovesRecordedPaths(t *testing.T) {
	_, gh := fakeGH(t)
	cfg := testConfig(t)
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), gh)

	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CSGODir(), "gameinfo.gi"), []byte("Game\tcsgo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Install(context.Background(), "weaponpaints", false); err != nil {
		t.Fatal(err)
	}
	dll := filepath.Join(cfg.CSGODir(), "addons/counterstrikesharp/plugins/WeaponPaints/WeaponPaints.dll")
	if !fileExists(dll) {
		t.Fatal("precondition: dll exists")
	}
	if err := in.Uninstall("weaponpaints"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if fileExists(dll) {
		t.Fatal("dll still exists after uninstall")
	}
	if in.IsInstalled("weaponpaints") {
		t.Fatal("state still recorded after uninstall")
	}
}

func TestUninstallRefusesEscape(t *testing.T) {
	cfg := testConfig(t)
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)
	// record a malicious manifest directly
	_ = store.SetPluginState(PluginState{
		Name:     "evil",
		Version:  "1",
		Status:   "installed",
		Manifest: map[string]string{"top0": "../../etc"},
	})
	if err := in.Uninstall("evil"); err == nil {
		t.Fatal("expected refusal to remove path outside csgo dir")
	}
}

func TestSafeSubPath(t *testing.T) {
	root := "/opt/cs2/game/csgo"
	if !safeSubPath(root, "/opt/cs2/game/csgo/addons") {
		t.Error("valid subdir rejected")
	}
	if safeSubPath(root, "/opt/cs2/game/csgo") {
		t.Error("root itself should not be removable")
	}
	if safeSubPath(root, "/opt/cs2/game/csgo/../..") {
		t.Error("traversal accepted")
	}
	if safeSubPath(root, "/etc/passwd") {
		t.Error("outside path accepted")
	}
}

func TestCatalogAnnotated(t *testing.T) {
	cfg := testConfig(t)
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)
	_ = store.SetPluginState(PluginState{Name: "metamod", Version: "v2", Status: "installed"})
	cat, err := in.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range cat {
		if e.ID == "metamod" {
			found = true
			if !strings.Contains(e.Description, "[installed v2]") {
				t.Fatalf("description not annotated: %q", e.Description)
			}
		}
	}
	if !found {
		t.Fatal("metamod missing from catalog")
	}
}
