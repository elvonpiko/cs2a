package fsatomic

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The bug this package exists to prevent: a plain temp-file-plus-rename replaces
// the destination with a fresh inode created at 0600, so the game server can no
// longer read its own server.cfg. rcon_password then stops being applied and RCON
// dies at the next map load — the panel's own "Repair RCON" could cause the
// failure it exists to fix.
func TestWriteKeepModePreservesPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.cfg")
	if err := os.WriteFile(path, []byte("hostname \"old\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An operator who tightened the file keeps that choice too.
	strict := filepath.Join(dir, "secrets.cfg")
	if err := os.WriteFile(strict, []byte("x\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := WriteKeepMode(path, []byte("hostname \"new\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Fatalf("mode = %o, want 644 (the game must still be able to read it)", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "hostname \"new\"\n" {
		t.Fatalf("content = %q", raw)
	}

	if err := WriteKeepMode(strict, []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(strict)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o640 {
		t.Fatalf("mode = %o, want 640 — the fallback must only apply to new files", perm)
	}
}

// The fallback mode is for a file that does not exist yet.
func TestWriteKeepModeUsesFallbackForANewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.cfg")
	if err := WriteKeepMode(path, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Fatalf("mode = %o, want the 644 fallback", perm)
	}
}

// Write is for files whose mode is part of the contract: a token file must stay
// unreadable to other accounts even if someone loosened it by hand.
func TestWriteEnforcesTheGivenMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte("{}"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

// No temp file may be left behind: the game tree is scanned by the installer's
// manifest logic and a stray .cs2a-* file in cfg/ would be served to the engine
// as config on some setups.
func TestWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.cfg")
	if err := WriteKeepMode(path, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "server.cfg" {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only server.cfg", names)
	}
}

// A failed write must not destroy the previous contents: the rename is the only
// step that publishes anything.
func TestWriteToAnUnwritableDirectoryLeavesTheOriginal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "server.cfg")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := WriteKeepMode(path, []byte("replacement\n"), 0o644); err == nil {
		t.Fatal("want an error when the temp file cannot be created")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "original\n" {
		t.Fatalf("content = %q, want the original to survive", raw)
	}
}

// Owner must not follow symlinks: reporting the target's owner would let the
// installer chown a file outside the game tree.
func TestOwnerDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	uid, gid, ok := Owner(link)
	if !ok {
		t.Fatal("Owner reported no ids for an existing symlink")
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(link, &st); err != nil {
		t.Fatal(err)
	}
	if uid != int(st.Uid) || gid != int(st.Gid) {
		t.Fatalf("Owner = %d:%d, lstat = %d:%d", uid, gid, st.Uid, st.Gid)
	}
	if _, _, ok := Owner(filepath.Join(dir, "missing")); ok {
		t.Error("a missing path must report ok=false")
	}
}
