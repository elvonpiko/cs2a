package cs2

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The game server runs unprivileged while the agent runs as root. Saving
// server.cfg used to replace it with a root-owned 0600 file, so the engine could
// no longer read its own config: rcon_password stopped being applied at the next
// map load and RCON died. The panel reported success, and "Repair RCON" was one
// of the buttons that triggered it.
func TestSaveServerCFGKeepsModeAndOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.cfg")
	if err := os.WriteFile(path, []byte("hostname \"before\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeUID, _, haveOwner := ownerOf(t, path)

	if err := SaveServerCFG(dir, "hostname \"after\"\n"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %v, want 0644 — the game user must still be able to read its config", got)
	}
	if haveOwner {
		if afterUID, _, _ := ownerOf(t, path); afterUID != beforeUID {
			t.Fatalf("owner changed from %d to %d", beforeUID, afterUID)
		}
	}
	got, err := LoadServerCFG(dir)
	if err != nil || got != "hostname \"after\"\n" {
		t.Fatalf("content = %q err=%v", got, err)
	}
}

// A hand-written server.cfg with restrictive permissions keeps them: cs2a must
// not quietly widen access to a file the operator locked down.
func TestSaveServerCFGPreservesTighterMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.cfg")
	if err := os.WriteFile(path, []byte("x\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := SaveServerCFG(dir, "y\n"); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want the operator's 0640 kept", fi.Mode().Perm())
	}
}

// A brand new server.cfg must be world-readable, or the game cannot read the
// file cs2a just created for it.
func TestSaveServerCFGCreatesReadableFile(t *testing.T) {
	dir := t.TempDir()
	if err := SaveServerCFG(dir, "hostname \"new\"\n"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "server.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", fi.Mode().Perm())
	}
}

// An operator who deletes or annotates the end marker must not lose the config
// below it. The old substring match appended a second block and then, on the
// next write, matched from the first BEGIN to the second END and deleted
// everything in between.
func TestApplyManagedBlockSurvivesADamagedEndMarker(t *testing.T) {
	user := "// my server\nmp_maxrounds 30\nsv_lan 1\nexec my_extra.cfg\n"
	out := ApplyManagedBlock(user, []CFGSetting{{Name: "hostname", Value: "A"}})
	out = strings.Replace(out, ManagedBlockEnd, "// end of the cs2a part", 1)

	for i, val := range []string{"B", "C", "D"} {
		out = ApplyManagedBlock(out, []CFGSetting{{Name: "hostname", Value: val}})
		for _, want := range []string{"// my server", "mp_maxrounds 30", "sv_lan 1", "exec my_extra.cfg"} {
			if !strings.Contains(out, want) {
				t.Fatalf("apply %d lost %q:\n%s", i+1, want, out)
			}
		}
		if got := ExtractManagedBlock(out); len(got) != 1 || got[0].Value != val {
			t.Fatalf("apply %d: extract = %+v", i+1, got)
		}
	}
}

// A stray BEGIN marker inside the block must not make the region between the two
// BEGIN markers disappear: the nearest pair wins.
func TestApplyManagedBlockWithNestedBeginMarker(t *testing.T) {
	content := ManagedBlockBegin + "\nmp_friendlyfire 1\n" + ManagedBlockBegin + "\nhostname \"x\"\n" + ManagedBlockEnd + "\n"
	out := ApplyManagedBlock(content, []CFGSetting{{Name: "hostname", Value: "y"}})
	if !strings.Contains(out, "mp_friendlyfire 1") {
		t.Fatalf("content before the nested marker was deleted:\n%s", out)
	}
	if strings.Contains(out, `hostname "x"`) {
		t.Fatalf("the innermost block was not replaced:\n%s", out)
	}
}

// A copied server.cfg with two complete blocks: only one is rewritten, and the
// operator's lines between them survive. Rewriting both would keep a block the
// UI cannot show in lockstep with one it can.
func TestApplyManagedBlockLeavesADuplicateBlockAlone(t *testing.T) {
	blk := ManagedBlockBegin + "\nhostname \"one\"\n" + ManagedBlockEnd
	content := blk + "\nmp_friendlyfire 1\n" + blk + "\n"
	out := ApplyManagedBlock(content, []CFGSetting{{Name: "hostname", Value: "two"}})
	if !strings.Contains(out, "mp_friendlyfire 1") {
		t.Fatalf("content between the two blocks was deleted:\n%s", out)
	}
	if n := strings.Count(out, ManagedBlockBegin); n != 2 {
		t.Fatalf("block count = %d, want the second block untouched:\n%s", n, out)
	}
	if !strings.Contains(out, `hostname "two"`) {
		t.Fatalf("the block was not updated:\n%s", out)
	}
}

// An END marker with no BEGIN before it is somebody else's line, not a block.
func TestApplyManagedBlockIgnoresAnOrphanEndMarker(t *testing.T) {
	content := "mp_maxrounds 16\n" + ManagedBlockEnd + "\nsv_lan 1\n"
	out := ApplyManagedBlock(content, []CFGSetting{{Name: "hostname", Value: "x"}})
	if !strings.Contains(out, "mp_maxrounds 16") || !strings.Contains(out, "sv_lan 1") {
		t.Fatalf("operator lines lost:\n%s", out)
	}
	if n := strings.Count(out, ManagedBlockBegin); n != 1 {
		t.Fatalf("want exactly one appended block, got %d:\n%s", n, out)
	}
}

// A comment is not a place to smuggle config from. A newline in Comment used to
// render its tail as an executable line inside the managed block, so an
// admin-level API caller could set rcon_password through a comment field.
func TestRenderManagedBlockCannotInjectALineViaComment(t *testing.T) {
	out := ApplyManagedBlock("", []CFGSetting{
		{Name: "hostname", Value: "x", Comment: "note\nrcon_password \"leaked\"\nsv_cheats 1"},
	})
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "//") {
			continue
		}
		if !strings.HasPrefix(l, "hostname ") {
			t.Fatalf("comment produced an executable line %q:\n%s", l, out)
		}
	}
	if got := ExtractManagedBlock(out); len(got) != 1 {
		t.Fatalf("extract = %+v", got)
	}
}

// Extraction has to read the lines the engine reads. Tab separators and bare
// cvars used to be dropped — and since the next write rebuilds the block from
// what was extracted, changing the server password silently deleted them.
func TestExtractManagedBlockKeepsAwkwardLines(t *testing.T) {
	content := ManagedBlockBegin + "\n" +
		"mp_maxrounds\t24\n" +
		"mp_autokick\n" +
		"sv_cheats \"0\" // left by a plugin doc\n" +
		"hostname   \"spaced   out\"\n" +
		ManagedBlockEnd + "\n"
	got := ExtractManagedBlock(content)
	want := []CFGSetting{
		{Name: "mp_maxrounds", Value: "24"},
		{Name: "mp_autokick", Bare: true},
		{Name: "sv_cheats", Value: "0"},
		{Name: "hostname", Value: "spaced   out"},
	}
	if len(got) != len(want) {
		t.Fatalf("extract = %+v, want %d entries", got, len(want))
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Value != want[i].Value || got[i].Bare != want[i].Bare {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Round-tripping must not lose them either.
	again := ExtractManagedBlock(ApplyManagedBlock(content, got))
	if len(again) != len(want) {
		t.Fatalf("round trip = %+v", again)
	}
}

// A value-less line is a query the engine answers by printing the current value.
// Writing it back as `mp_autokick ""` sets the cvar to 0 — and the settings save
// pushes that live over RCON — so it must round-trip as the bare line it was.
func TestBareCvarLineIsNotTurnedIntoAnAssignment(t *testing.T) {
	content := ManagedBlockBegin + "\nmp_autokick\nsv_cheats \"0\"\n" + ManagedBlockEnd + "\n"
	got := ExtractManagedBlock(content)
	if len(got) != 2 || !got[0].Bare {
		t.Fatalf("extract = %+v", got)
	}
	out := ApplyManagedBlock(content, got)
	for _, bad := range []string{`mp_autokick ""`, `mp_autokick "0"`, `mp_autokick "`} {
		if strings.Contains(out, bad) {
			t.Fatalf("bare cvar rendered as %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "\nmp_autokick\n") {
		t.Fatalf("bare cvar lost:\n%s", out)
	}
	// Stable across repeated writes.
	if out2 := ApplyManagedBlock(out, ExtractManagedBlock(out)); out2 != out {
		t.Fatalf("second write changed the block:\n%s\n---\n%s", out, out2)
	}
}

// A bare cvar with a trailing comment is still a query.
func TestBareCvarWithTrailingComment(t *testing.T) {
	got := ExtractManagedBlock(ManagedBlockBegin + "\nmp_autokick  // why\n" + ManagedBlockEnd)
	if len(got) != 1 || !got[0].Bare || got[0].Name != "mp_autokick" {
		t.Fatalf("extract = %+v", got)
	}
}

// A CRLF server.cfg stays CRLF, and extracted values carry no stray \r (which
// would be pushed live over RCON as part of the value).
func TestManagedBlockPreservesCRLF(t *testing.T) {
	user := "// header\r\nmp_maxrounds 16\r\n"
	out := ApplyManagedBlock(user, []CFGSetting{{Name: "hostname", Value: "X", Comment: "managed by cs2a"}})
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Fatalf("mixed line endings:\n%q", out)
	}
	got := ExtractManagedBlock(out)
	if len(got) != 1 || got[0].Value != "X" || strings.Contains(got[0].Value, "\r") {
		t.Fatalf("extract = %#v", got)
	}
	if got[0].Comment != "managed by cs2a" {
		t.Fatalf("comment = %q", got[0].Comment)
	}
	// A second apply keeps the endings consistent.
	out2 := ApplyManagedBlock(out, got)
	if strings.Contains(strings.ReplaceAll(out2, "\r\n", ""), "\n") {
		t.Fatalf("mixed line endings after re-apply:\n%q", out2)
	}
}

// The file must keep its trailing newline: replacing a block that sits at EOF
// used to leave a file with none, which makes every backup diff churn.
func TestApplyManagedBlockEndsWithANewline(t *testing.T) {
	out := ApplyManagedBlock("", []CFGSetting{{Name: "hostname", Value: "a"}})
	for i := 0; i < 3; i++ {
		if !strings.HasSuffix(out, "\n") {
			t.Fatalf("apply %d produced no trailing newline:\n%q", i, out)
		}
		out = ApplyManagedBlock(out, []CFGSetting{{Name: "hostname", Value: "b"}})
	}
}

// ownerOf reports a path's uid/gid, or ok=false where that is not observable.
func ownerOf(t *testing.T, path string) (uid, gid int, ok bool) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, isUnix := fi.Sys().(*syscall.Stat_t)
	if !isUnix {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
