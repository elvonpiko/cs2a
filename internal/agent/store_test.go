package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorePluginLifecycle(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.GetPluginState("metamod"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetPluginState(PluginState{
		Name:     "metamod",
		Version:  "v2.0.0",
		Status:   "installed",
		Manifest: map[string]string{"addons": "/opt/cs2/game/csgo/addons/metamod"},
	}))
	got, err := s.GetPluginState("metamod")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != "v2.0.0" || got.Status != "installed" || got.Manifest["addons"] == "" {
		t.Fatalf("got %+v", got)
	}
	if got.InstalledAt.IsZero() {
		t.Fatal("installed_at not set")
	}

	// upsert
	must(s.SetPluginState(PluginState{Name: "metamod", Version: "v2.1.0", Status: "installed"}))
	got, _ = s.GetPluginState("metamod")
	if got.Version != "v2.1.0" {
		t.Fatalf("upsert failed: %+v", got)
	}

	must(s.SetPluginState(PluginState{Name: "cssharp", Version: "v332", Status: "installed"}))
	list, err := s.ListPluginStates()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].Name != "cssharp" {
		t.Fatalf("list = %+v", list)
	}

	must(s.DeletePluginState("cssharp"))
	if _, err := s.GetPluginState("cssharp"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if err := s.DeletePluginState("cssharp"); err != ErrNotFound {
		t.Fatalf("delete of missing should be ErrNotFound, got %v", err)
	}
}

func TestStoreMeta(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.GetMeta("nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.SetMeta("k", "v1"); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetMeta("k")
	if v != "v1" {
		t.Fatalf("got %q", v)
	}
	if err := s.SetMeta("k", "v2"); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetMeta("k")
	if v != "v2" {
		t.Fatalf("upsert got %q", v)
	}
}

func TestOpenStoreCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	s, err := OpenStore(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("open nested: %v", err)
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(dir, "x.db")); err != nil {
		t.Fatalf("db missing: %v", err)
	}
}
