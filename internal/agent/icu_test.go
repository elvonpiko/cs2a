package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubLdconfig writes a fake ldconfig whose -p output is the given text, and
// returns a PATH that finds it.
func stubLdconfig(t *testing.T, output string, missing bool) string {
	t.Helper()
	dir := t.TempDir()
	name := "ldconfig"
	if missing {
		// an ldconfig that exits 1: "no ldconfig" paths
		os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	} else {
		os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\ncat <<'OUT'\n"+output+"\nOUT\n"), 0o755)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func TestCheckICU(t *testing.T) {
	cases := []struct {
		name   string
		output string
		miss   bool
		want   string // "" = clean
	}{
		{"present", "\tlibicuuc.so.76 => /lib/x86_64-linux-gnu/libicuuc.so.76", false, ""},
		{"absent", "\tlibc.so.6 => /lib/x86_64-linux-gnu/libc.so.6", false, "libicuuc is missing"},
		{"no ldconfig", "", true, ""}, // cannot judge: not a failure
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", stubLdconfig(t, tc.output, tc.miss))
			err := checkICU()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want clean, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}
