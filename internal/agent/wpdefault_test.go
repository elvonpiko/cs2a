package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The installer provisions the WeaponPaints database and knows its DSN, so the
// generated config must already contain the credentials. They used to be printed
// once in the installer's output and had to be retyped by hand; miss that and
// the plugin loads with an empty DatabaseHost and no skin ever applies.
func TestWeaponPaintsDefaultConfigFillsDatabaseCredentials(t *testing.T) {
	cfg := testConfig(t)
	cfg.WPDsn = "cs2a:s3cr#t@tcp(127.0.0.1:3307)/cs2_wp"
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)

	if err := in.writeWeaponPaintsDefaultConfig(); err != nil {
		t.Fatalf("write: %v", err)
	}
	path := filepath.Join(cfg.CSGODir(),
		"addons", "counterstrikesharp", "configs", "plugins", "WeaponPaints", "WeaponPaints.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("config is not valid JSON: %v\n%s", err, raw)
	}
	if doc["DatabaseHost"] != "127.0.0.1" {
		t.Errorf("DatabaseHost = %v", doc["DatabaseHost"])
	}
	if doc["DatabasePort"] != float64(3307) {
		t.Errorf("DatabasePort = %v", doc["DatabasePort"])
	}
	if doc["DatabaseUser"] != "cs2a" {
		t.Errorf("DatabaseUser = %v", doc["DatabaseUser"])
	}
	if doc["DatabasePassword"] != "s3cr#t" {
		t.Errorf("DatabasePassword = %v", doc["DatabasePassword"])
	}
	if doc["DatabaseName"] != "cs2_wp" {
		t.Errorf("DatabaseName = %v", doc["DatabaseName"])
	}
	// The rest of the default must survive.
	if doc["ConfigVersion"] != float64(10) {
		t.Errorf("ConfigVersion = %v", doc["ConfigVersion"])
	}
	add, ok := doc["Additional"].(map[string]any)
	if !ok || add["KnifeEnabled"] != true {
		t.Errorf("Additional block lost: %v", doc["Additional"])
	}
}

// An admin-edited config must never be overwritten by a reinstall.
func TestWeaponPaintsDefaultConfigNeverClobbers(t *testing.T) {
	cfg := testConfig(t)
	cfg.WPDsn = "cs2a:pw@tcp(127.0.0.1:3306)/cs2_wp"
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)

	path := filepath.Join(cfg.CSGODir(),
		"addons", "counterstrikesharp", "configs", "plugins", "WeaponPaints", "WeaponPaints.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := `{"ConfigVersion":10,"DatabaseHost":"db.internal"}`
	if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := in.writeWeaponPaintsDefaultConfig(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != mine {
		t.Fatalf("existing config was modified:\n%s", raw)
	}
}

// A malformed wp_dsn must not fail the install: the config is still written
// (the plugin needs the file) and the operator is told to fill the credentials
// in from the panel.
func TestWeaponPaintsDefaultConfigBadDSNIsAWarning(t *testing.T) {
	cfg := testConfig(t)
	cfg.WPDsn = "not a dsn"
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)

	err = in.writeWeaponPaintsDefaultConfig()
	if _, ok := asWarning(err); !ok {
		t.Fatalf("expected a warning, got %v", err)
	}
	path := filepath.Join(cfg.CSGODir(),
		"addons", "counterstrikesharp", "configs", "plugins", "WeaponPaints", "WeaponPaints.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the config must still be written: %v", err)
	}
	if !strings.Contains(string(raw), `"ConfigVersion": 10`) {
		t.Fatalf("default config not written:\n%s", raw)
	}
}

// WeaponPaints resolves gamedata two directories above its module dir, so the
// file the archive ships at its own root has to be lifted out of the plugins
// directory. Without this the plugin installs "successfully" and then calls
// Unload(false) on every boot.
func TestPlaceWeaponPaintsGamedata(t *testing.T) {
	cfg := testConfig(t)
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)

	cssharp := filepath.Join(cfg.CSGODir(), "addons", "counterstrikesharp")
	stray := filepath.Join(cssharp, "plugins", "gamedata")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "weaponpaints.json"), []byte(`{"a":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := in.placeWeaponPaintsGamedata(); err != nil {
		t.Fatalf("place: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(cssharp, "gamedata", "weaponpaints.json"))
	if err != nil {
		t.Fatalf("gamedata not placed: %v", err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("content = %q", got)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("the stray plugins/gamedata dir must be removed")
	}
}

// The copy inside the plugin folder is preferred, since it exists even if
// upstream ever drops the stray root copy.
func TestPlaceWeaponPaintsGamedataPrefersPluginFolder(t *testing.T) {
	cfg := testConfig(t)
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)

	cssharp := filepath.Join(cfg.CSGODir(), "addons", "counterstrikesharp")
	inPlugin := filepath.Join(cssharp, "plugins", "WeaponPaints", "gamedata")
	if err := os.MkdirAll(inPlugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inPlugin, "weaponpaints.json"), []byte(`{"from":"plugin"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(cssharp, "plugins", "gamedata")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "weaponpaints.json"), []byte(`{"from":"root"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := in.placeWeaponPaintsGamedata(); err != nil {
		t.Fatalf("place: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(cssharp, "gamedata", "weaponpaints.json"))
	if string(got) != `{"from":"plugin"}` {
		t.Fatalf("content = %q", got)
	}
}

// A release that ships no gamedata file must degrade to a warning: the install
// itself succeeded and its files are recorded, so failing it would leave state
// the operator cannot uninstall.
func TestPlaceWeaponPaintsGamedataMissingIsAWarning(t *testing.T) {
	cfg := testConfig(t)
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)

	err = in.placeWeaponPaintsGamedata()
	if err == nil {
		t.Fatal("expected a warning")
	}
	msg, ok := asWarning(err)
	if !ok {
		t.Fatalf("must be a non-fatal warning, got %T: %v", err, err)
	}
	if !strings.Contains(msg, "weaponpaints.json") {
		t.Fatalf("warning = %q", msg)
	}
}
