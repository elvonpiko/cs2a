package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mmArtifact is the versioned metamod filename the pointer file names.
const mmArtifact = "mmsource-2.0.0-git1411-linux.tar.gz"

// fakeGH serves GitHub-API-like release metadata and a downloadable asset.
func fakeGH(t *testing.T) (*httptest.Server, *GHClient) {
	t.Helper()
	base := ""
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/roflmuffin/CounterStrikeSharp/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GHRelease{
			TagName: "v100.0.0",
			Assets: []GHAsset{
				{Name: "counterstrikesharp-windows-100.0.0.zip", URL: base + "/assets/cssharp-win.zip"},
				{Name: "counterstrikesharp-with-runtime-linux-100.0.0.zip", URL: base + "/assets/cssharp-runtime.zip"},
			},
		})
	})
	mux.HandleFunc("/repos/Nereziel/cs2-WeaponPaints/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(GHRelease{
			TagName: "build-459",
			Assets: []GHAsset{
				// the website bundle must never be picked for a game server
				{Name: "WeaponPaints-Website.zip", URL: base + "/assets/wp-site.zip"},
				{Name: "WeaponPaints.zip", URL: base + "/assets/wp.zip"},
			},
		})
	})
	// AlliedModders publishes a pointer file naming the current build; the
	// versioned tarball sits next to it. Serving both is what makes the
	// two-step resolution testable.
	mux.HandleFunc("/mmsdrop/2.0/mmsource-latest-linux", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, mmArtifact+"\n")
	})
	mux.HandleFunc("/mmsdrop/2.0/"+mmArtifact, func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gw)
		body := "ELF"
		_ = tw.WriteHeader(&tar.Header{Name: "addons/metamod/bin/linuxsteamrt64/metamod.2.cs2.so", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
		_ = tw.WriteHeader(&tar.Header{Name: "addons/metamod.vdf", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
		tw.Write([]byte("vdf\n"))
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
	// the real WeaponPaints.zip holds a bare WeaponPaints/ folder plus a second
	// top-level gamedata/ dir — the installer must place the folder under the
	// cssharp plugins dir and lift gamedata to where the plugin reads it
	mux.HandleFunc("/assets/wp.zip", func(w http.ResponseWriter, r *http.Request) {
		zipBytes, _ := makeZip(map[string][]byte{
			"WeaponPaints/WeaponPaints.dll":           {9},
			"WeaponPaints/lang/en.json":               []byte("{}"),
			"WeaponPaints/gamedata/weaponpaints.json": []byte(`{"gamedata":1}`),
			"gamedata/weaponpaints.json":              []byte(`{"gamedata":1}`),
		})
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipBytes)
	})
	mux.HandleFunc("/assets/wp-site.zip", func(w http.ResponseWriter, r *http.Request) {
		t.Error("installer downloaded the WeaponPaints website bundle")
		w.WriteHeader(http.StatusInternalServerError)
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
	if res.Version != "build-459" {
		t.Fatalf("version = %q", res.Version)
	}

	// files extracted into csgo dir
	dll := filepath.Join(cfg.CSGODir(), "addons/counterstrikesharp/plugins/WeaponPaints/WeaponPaints.dll")
	if !fileExists(dll) {
		t.Fatal("plugin dll missing")
	}
	// WeaponPaints refuses to load unless gamedata sits exactly two levels
	// above its module dir; the archive ships it at its own root instead.
	gamedata := filepath.Join(cfg.CSGODir(), "addons/counterstrikesharp/gamedata/weaponpaints.json")
	if !fileExists(gamedata) {
		t.Fatal("gamedata/weaponpaints.json was not lifted out of the plugin folder")
	}
	if fileExists(filepath.Join(cfg.CSGODir(), "addons/counterstrikesharp/plugins/gamedata")) {
		t.Fatal("stray plugins/gamedata directory left behind")
	}
	if res.Warning != "" {
		t.Fatalf("unexpected warning: %s", res.Warning)
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
	// Owns is authoritative: uninstalling must not delete the shared addons/
	// tree that every other plugin lives in.
	st, _ := store.GetPluginState("weaponpaints")
	if st.Manifest["top0"] != "addons/counterstrikesharp/plugins/WeaponPaints" {
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
	if res2.Version != "build-459" {
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
	// the version comes from the pointer file, not a hardcoded "latest"
	if res.Version != "2.0.0-git1411" || res.RequiresRestart != true {
		t.Fatalf("res = %+v", res)
	}
	raw, _ := os.ReadFile(gi)
	if !bytes.Contains(raw, []byte("csgo/addons/metamod")) {
		t.Fatalf("gameinfo not patched:\n%s", raw)
	}
	so := filepath.Join(cfg.CSGODir(), "addons/metamod/bin/linuxsteamrt64/metamod.2.cs2.so")
	if !fileExists(so) {
		t.Fatal("metamod file missing")
	}
	if !fileExists(filepath.Join(cfg.CSGODir(), "addons/metamod.vdf")) {
		t.Fatal("metamod.vdf missing")
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

// Removing Metamod:Source out from under CounterStrikeSharp used to succeed and
// silently break every plugin on the server.
func TestUninstallRefusesWhenSomethingDependsOnIt(t *testing.T) {
	cfg := testConfig(t)
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)
	for _, id := range []string{"metamod", "cssharp"} {
		if err := store.SetPluginState(PluginState{
			Name: id, Version: "v1", Status: "installed",
			Manifest: map[string]string{"top0": "addons/" + id},
		}); err != nil {
			t.Fatal(err)
		}
	}

	err := in.Uninstall("metamod")
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "CounterStrikeSharp") {
		t.Fatalf("the error must name the blocker: %v", err)
	}
	if !in.IsInstalled("metamod") {
		t.Fatal("state must be untouched after a refusal")
	}

	// With the dependent gone, metamod may be removed.
	if err := in.Uninstall("cssharp"); err != nil {
		t.Fatalf("uninstall cssharp: %v", err)
	}
	if err := in.Uninstall("metamod"); err != nil {
		t.Fatalf("uninstall metamod: %v", err)
	}
}

// Uninstalling metamod must also remove the gameinfo.gi search path it added;
// the engine otherwise complains about a missing path on every boot.
func TestUninstallRevertsGameinfoPatch(t *testing.T) {
	cfg := testConfig(t)
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)
	if err := os.MkdirAll(cfg.CSGODir(), 0o755); err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(cfg.CSGODir(), "gameinfo.gi")
	if err := os.WriteFile(gi, []byte("\t\t\tGame\tcsgo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patchGameinfoMetamod(gi); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPluginState(PluginState{
		Name: "metamod", Version: "v1", Status: "installed",
		Manifest: map[string]string{"top0": "addons/metamod"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := in.Uninstall("metamod"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	raw, err := os.ReadFile(gi)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "csgo/addons/metamod") {
		t.Fatalf("search path not removed:\n%s", raw)
	}
	if !strings.Contains(string(raw), "Game\tcsgo") {
		t.Fatalf("the original line was destroyed:\n%s", raw)
	}
}

// The revert must be idempotent and must not touch an operator's comment.
func TestUnpatchGameinfoMetamod(t *testing.T) {
	dir := t.TempDir()
	gi := filepath.Join(dir, "gameinfo.gi")
	body := "\t\t\t// we use csgo/addons/metamod here\n" +
		"\t\t\tGame\tcsgo/addons/metamod\n" +
		"\t\t\tGame\tcsgo\n"
	if err := os.WriteFile(gi, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := unpatchGameinfoMetamod(gi); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(gi)
	got := string(raw)
	if strings.Contains(got, "Game\tcsgo/addons/metamod") {
		t.Fatalf("search path survived:\n%s", got)
	}
	if !strings.Contains(got, "// we use csgo/addons/metamod here") {
		t.Fatalf("the operator's comment was removed:\n%s", got)
	}
	// A missing file is not an error: the game may already be gone.
	if err := unpatchGameinfoMetamod(filepath.Join(dir, "nope.gi")); err != nil {
		t.Fatalf("missing gameinfo.gi: %v", err)
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

// Installed state must ride typed fields. It used to be spliced into
// Description as "[installed v2] …" and parsed back out by the panel, which
// mislabelled any version string containing a "]".
func TestCatalogReportsInstalledState(t *testing.T) {
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
		switch e.ID {
		case "metamod":
			found = true
			if !e.Installed || e.InstalledVersion != "v2" {
				t.Fatalf("metamod = %+v", e)
			}
			if strings.Contains(e.Description, "installed") {
				t.Fatalf("description must stay untouched: %q", e.Description)
			}
		default:
			if e.Installed {
				t.Errorf("%s reported as installed", e.ID)
			}
		}
	}
	if !found {
		t.Fatal("metamod missing from catalog")
	}
}

// HasConfig decides whether a plugin card offers a config editor; every card
// used to claim one.
func TestCatalogHasConfig(t *testing.T) {
	byID := map[string]CatalogEntry{}
	for _, e := range DefaultCatalog() {
		byID[e.ID] = e
	}
	if byID["weaponpaints"].HasConfig() != true {
		t.Error("weaponpaints has a config file")
	}
	if byID["metamod"].HasConfig() != false {
		t.Error("metamod has no JSON config the panel can edit")
	}
}

// Every catalog entry must be internally consistent: the asset pattern has to
// compile, Owns must be present (an uninstall with no recorded paths would
// silently leave files behind), and Dest/Owns must agree.
func TestCatalogEntriesWellFormed(t *testing.T) {
	ids := map[string]bool{}
	for _, e := range DefaultCatalog() {
		if ids[e.ID] {
			t.Errorf("duplicate catalog id %q", e.ID)
		}
		ids[e.ID] = true
		if e.Name == "" || e.Description == "" || e.Homepage == "" {
			t.Errorf("%s: missing name/description/homepage", e.ID)
		}
		switch e.Kind {
		case KindRuntime, KindMetamodPlugin, KindCSSharpPlugin:
		default:
			t.Errorf("%s: unknown kind %q", e.ID, e.Kind)
		}
		if e.Repo == "" && e.URL == "" {
			t.Errorf("%s: neither repo nor url", e.ID)
		}
		if e.Repo != "" {
			if e.AssetRegex == "" {
				t.Errorf("%s: repo without asset regex", e.ID)
			}
			if _, err := regexp.Compile(e.AssetRegex); err != nil {
				t.Errorf("%s: asset regex: %v", e.ID, err)
			}
		}
		if e.AssetReject != "" {
			if _, err := regexp.Compile(e.AssetReject); err != nil {
				t.Errorf("%s: asset reject regex: %v", e.ID, err)
			}
		}
		if len(e.Owns) == 0 {
			t.Errorf("%s: no owned paths — uninstall would be a no-op", e.ID)
		}
		for _, p := range e.Owns {
			if strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
				t.Errorf("%s: owned path %q must be csgo-relative", e.ID, p)
			}
		}
		if strings.HasPrefix(e.Dest, "/") || strings.Contains(e.Dest, "..") {
			t.Errorf("%s: dest %q must be csgo-relative", e.ID, e.Dest)
		}
		for _, dep := range e.Requires {
			if _, ok := Find(DefaultCatalog(), dep); !ok {
				t.Errorf("%s: requires unknown entry %q", e.ID, dep)
			}
		}
	}
	// the two runtime layers everything else depends on must exist
	for _, id := range []string{"metamod", "cssharp"} {
		if !ids[id] {
			t.Errorf("catalog missing %s", id)
		}
	}
}

// An upstream release that grows a second matching asset must fail loudly
// rather than install a Windows build or a bundle that duplicates a dependency.
func TestResolveArtifactRejectsAmbiguity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/shobhit-pathak/MatchZy/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(GHRelease{
			TagName: "1.0.0",
			Assets: []GHAsset{
				{Name: "MatchZy-1.0.0-with-cssharp-linux.zip", URL: "http://x/a.zip"},
				{Name: "MatchZy-1.0.0-with-cssharp-windows.zip", URL: "http://x/b.zip"},
				{Name: "MatchZy-1.0.0.zip", URL: "http://x/c.zip"},
			},
		})
	}))
	defer srv.Close()

	cfg := testConfig(t)
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	gh := NewGHClient("")
	gh.HTTP.Transport = rewriteTransport{base: http.DefaultTransport, to: srv.URL}
	in := NewInstaller(cfg, store, DefaultCatalog(), gh)

	// AssetReject narrows the three assets down to the plain one.
	entry, _ := Find(DefaultCatalog(), "matchzy")
	name, urls, version, err := in.resolveArtifact(context.Background(), entry)
	if err != nil {
		t.Fatalf("resolve with reject: %v", err)
	}
	if name != "MatchZy-1.0.0.zip" || version != "1.0.0" ||
		len(urls) != 1 || urls[0] != "http://x/c.zip" {
		t.Fatalf("picked %q (%v) @ %q", name, urls, version)
	}

	// Without it, the ambiguity must be reported instead of guessed.
	entry.AssetReject = ""
	if _, _, _, err = in.resolveArtifact(context.Background(), entry); err == nil {
		t.Fatal("expected an error for 3 matching assets")
	} else if !strings.Contains(err.Error(), "narrower pattern") {
		t.Fatalf("unhelpful error: %v", err)
	}
}
