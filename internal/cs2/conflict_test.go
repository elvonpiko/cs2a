package cs2

import (
	"strings"
	"testing"
)

// A server.cfg that was copied between installs, or edited by hand, can end up
// with two cs2a blocks. cs2a rewrites the first one, but the engine executes both
// in order — so the second block's values are what the server actually runs, and
// the panel shows the first. That disagreement is invisible without a warning.
func TestManagedBlockConflictIsReported(t *testing.T) {
	one := ManagedBlockBegin + "\nmp_maxrounds \"24\"\n" + ManagedBlockEnd + "\n"
	two := one + "\nhostname \"mine\"\n\n" + ManagedBlockBegin + "\nmp_maxrounds \"16\"\n" + ManagedBlockEnd + "\n"

	if n := CountManagedBlocks(""); n != 0 {
		t.Errorf("empty cfg has %d blocks", n)
	}
	if n := CountManagedBlocks("hostname \"mine\"\n"); n != 0 {
		t.Errorf("plain cfg has %d blocks", n)
	}
	if n := CountManagedBlocks(one); n != 1 {
		t.Errorf("single block counted as %d", n)
	}
	if n := CountManagedBlocks(two); n != 2 {
		t.Fatalf("two blocks counted as %d", n)
	}

	if warn := ManagedBlockConflict(one); warn != "" {
		t.Errorf("a single block is not a conflict: %q", warn)
	}
	warn := ManagedBlockConflict(two)
	if warn == "" {
		t.Fatal("a duplicate block must be reported")
	}
	for _, want := range []string{"2", "server.cfg"} {
		if !strings.Contains(warn, want) {
			t.Errorf("warning %q should mention %q", warn, want)
		}
	}

	// The warning must not come with a behaviour change: cs2a still edits only
	// the first block, leaving the second exactly as the operator left it.
	out := ApplyManagedBlock(two, []CFGSetting{{Name: "mp_maxrounds", Value: "30"}})
	if CountManagedBlocks(out) != 2 {
		t.Fatalf("the second block was consumed:\n%s", out)
	}
	if !strings.Contains(out, `mp_maxrounds "30"`) {
		t.Errorf("the first block was not updated:\n%s", out)
	}
	if !strings.Contains(out, `mp_maxrounds "16"`) {
		t.Errorf("the second block was modified:\n%s", out)
	}
	if !strings.Contains(out, `hostname "mine"`) {
		t.Errorf("operator config between the blocks was lost:\n%s", out)
	}
}

// An orphan end marker (someone deleted the BEGIN line) is not a block, so it
// must not be counted as one — otherwise every save would warn about a conflict
// that does not exist.
func TestCountManagedBlocksIgnoresOrphanMarkers(t *testing.T) {
	if n := CountManagedBlocks("hostname \"x\"\n" + ManagedBlockEnd + "\n"); n != 0 {
		t.Errorf("orphan END counted as %d blocks", n)
	}
	if n := CountManagedBlocks(ManagedBlockBegin + "\nmp_autokick\n"); n != 0 {
		t.Errorf("unterminated block counted as %d blocks", n)
	}
	// A nested BEGIN is still one block: the nearest pair is what cs2a rewrites.
	nested := ManagedBlockBegin + "\n" + ManagedBlockBegin + "\nsv_cheats \"0\"\n" + ManagedBlockEnd + "\n"
	if n := CountManagedBlocks(nested); n != 1 {
		t.Errorf("nested BEGIN counted as %d blocks", n)
	}
}
