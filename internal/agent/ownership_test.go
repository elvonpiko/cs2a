package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The game tree's owner is the authority for who must own newly installed
// files: the agent runs as root, the game does not.
func TestTreeOwnerReadsTheGameTree(t *testing.T) {
	dir := t.TempDir()
	uid, gid, ok := treeOwner(dir)
	if !ok {
		t.Fatal("ownership of a real directory must be readable")
	}
	if uid != os.Getuid() || gid != os.Getgid() {
		t.Fatalf("uid/gid = %d/%d, want %d/%d", uid, gid, os.Getuid(), os.Getgid())
	}
	if _, _, ok := treeOwner(filepath.Join(dir, "missing")); ok {
		t.Fatal("a missing directory must report ok=false")
	}
}

// alignOwnership is a no-op for a non-root agent: every file it wrote already
// belongs to the right user, and chown would fail.
func TestAlignOwnershipNoopWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test asserts the non-root path")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// uid 12345 does not exist; a real chown attempt would fail loudly.
	if err := alignOwnership(dir, 12345, 12345); err != nil {
		t.Fatalf("must not attempt chown as a non-root agent: %v", err)
	}
}

// applyGameOwnership must never follow a manifest path out of the game tree,
// and must tolerate Owns entries a given release did not ship.
func TestApplyGameOwnershipIgnoresEscapesAndMissingPaths(t *testing.T) {
	cfg := testConfig(t)
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)

	if err := os.MkdirAll(cfg.CSGODir(), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(cfg.CSGODir(), "addons")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	// A path outside the tree that would be catastrophic to chown.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	warning := in.applyGameOwnership([]string{
		"addons",
		"addons/never-shipped-by-this-release",
		"../../../../" + strings.TrimPrefix(outside, "/"),
	})
	if warning != "" {
		t.Fatalf("unexpected warning: %s", warning)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("path outside the game tree was disturbed: %v", err)
	}
}

// A game tree owned by root needs no alignment at all.
func TestApplyGameOwnershipSkipsRootOwnedTree(t *testing.T) {
	cfg := testConfig(t)
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)
	// The unit reports no User=, so there is no account to hand files to.
	in.sysd = fakeUnitUser("")
	// CSGODir does not exist: treeOwner reports not-ok, which must be handled
	// as "nothing to do" rather than as a failure.
	if got := in.applyGameOwnership([]string{"addons"}); got != "" {
		t.Fatalf("warning = %q", got)
	}
}

// fakeUnitUser stands in for systemd in ownership tests.
type fakeUnitUser string

func (f fakeUnitUser) UnitUser() string { return string(f) }

// A root-owned game tree is the case that broke plugins on a real VPS: steamcmd
// had been run as root, so csgo/ was root-owned, but the game ran as an
// unprivileged account and could not write the configs CounterStrikeSharp
// generates. The unit's User= is the only remaining signal, so it must be used.
func TestGameOwnerFallsBackToTheUnitUser(t *testing.T) {
	cfg := testConfig(t)
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)
	if err := os.MkdirAll(cfg.CSGODir(), 0o755); err != nil {
		t.Fatal(err)
	}

	// Running as a normal user, the tree already has a non-root owner and no
	// lookup should happen at all.
	if os.Geteuid() != 0 {
		in.sysd = fakeUnitUser("nobody")
		uid, _, ok := in.gameOwner()
		if !ok || uid != os.Getuid() {
			t.Fatalf("the game tree's own owner must win: uid=%d ok=%v", uid, ok)
		}
	}

	// An unknown account cannot be resolved, and inventing a uid would chown
	// the tree to nobody in particular.
	in.sysd = fakeUnitUser("cs2a-no-such-user")
	inRoot := *in
	inRoot.cfg.CS2Dir = filepath.Join(t.TempDir(), "missing")
	if _, _, ok := inRoot.gameOwner(); ok {
		t.Fatal("an unresolvable User= must not produce an owner")
	}

	// "root" is not an answer either: it is what the agent already is.
	inRoot.sysd = fakeUnitUser("root")
	if _, _, ok := inRoot.gameOwner(); ok {
		t.Fatal("User=root must not be treated as a game owner")
	}
}

// atomicWrite replaces a file by rename, which means a brand new inode owned by
// the agent — root. Rewriting a plugin config used to strip the game user's
// ownership, so cssharp could no longer update its own file. The temp file must
// inherit the destination's owner before the rename.
func TestAtomicWritePreservesOwnership(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "core.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _, ok := statOwner(path)
	if !ok {
		t.Skip("ownership not observable on this platform")
	}
	if err := atomicWrite(path, []byte(`{"k":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _, _ := statOwner(path)
	if after != before {
		t.Fatalf("owner changed from %d to %d across an atomic write", before, after)
	}
	// The content and mode must still be what was asked for.
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != `{"k":1}` {
		t.Fatalf("content = %q err=%v", raw, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

// A non-root agent must not attempt to give files away: the chown would fail and
// every write it guards would start reporting errors.
func TestOwnerToInheritIsInertWithoutRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test asserts the non-root path")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ownerToInherit(path); ok {
		t.Fatal("a non-root agent has nothing to inherit")
	}
}

// A file that does not exist yet takes the owner of the directory it lands in,
// which is how a newly created plugin config ends up writable by the game.
func TestOwnerToInheritUsesTheParentForNewFiles(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("only root can observe a meaningful answer here")
	}
	dir := t.TempDir()
	if err := os.Chown(dir, 12345, 12345); err != nil {
		t.Skipf("cannot chown the fixture: %v", err)
	}
	uid, gid, ok := ownerToInherit(filepath.Join(dir, "not-yet"))
	if !ok || uid != 12345 || gid != 12345 {
		t.Fatalf("uid/gid = %d/%d ok=%v, want 12345/12345", uid, gid, ok)
	}
}
