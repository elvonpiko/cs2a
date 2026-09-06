package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMapEnvWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cs2a-map")
	if err := WriteMapEnv(path, "de_cache"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "CS2A_MAP=de_cache\n" {
		t.Fatalf("env file = %q", b)
	}
	if got := readMapEnv(path); got != "de_cache" {
		t.Fatalf("read = %q", got)
	}
	// rewriting is atomic and keeps the destination mode
	if err := WriteMapEnv(path, "de_mirage"); err != nil {
		t.Fatal(err)
	}
	if got := readMapEnv(path); got != "de_mirage" {
		t.Fatalf("rewrite = %q", got)
	}
}

// The EnvironmentFile the game unit reads must not be able to vanish silently:
// EnsureMapEnv creates it with the default map when missing and leaves an
// existing one (and the operator's current map) alone.
func TestMapEnvEnsure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "cs2a-map")
	if err := EnsureMapEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := readMapEnv(path); got != DefaultMap {
		t.Fatalf("ensured = %q, want %q", got, DefaultMap)
	}
	// an existing file survives untouched
	if err := WriteMapEnv(path, "de_nuke"); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, before, before); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMapEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := readMapEnv(path); got != "de_nuke" {
		t.Fatalf("ensure clobbered the current map: %q", got)
	}
}

// A map name systemd would happily put on the ExecStart line but CS2 would
// choke on (or worse, a shell metacharacter) must never reach the env file.
func TestMapEnvRejectsBadNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cs2a-map")
	if err := WriteMapEnv(path, "de_dust2; rm -rf /"); err == nil {
		t.Fatal("invalid map name accepted into the env file")
	}
	if err := WriteMapEnv(path, ""); err == nil {
		t.Fatal("empty map name accepted")
	}
	if got := readMapEnv(path); got != "" {
		t.Fatalf("rejected write must not touch the file, got %q", got)
	}
}

// readMapEnv of a missing or malformed file reports nothing rather than
// failing: callers treat "" as "use the unit default".
func TestMapEnvReadMissing(t *testing.T) {
	if got := readMapEnv(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Fatalf("missing file = %q", got)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cs2a-map")
	if err := os.WriteFile(path, []byte("garbage without a key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readMapEnv(path); got != "" {
		t.Fatalf("garbage = %q", got)
	}
}
