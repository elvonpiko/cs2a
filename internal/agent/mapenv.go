package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The game unit launches CS2 with "+map ${CS2A_MAP}" and reads the variable
// from an EnvironmentFile cs2a owns. Without it the unit hardcoded de_dust2,
// so every restart dragged the server back to de_dust2 no matter what map the
// operator had switched to — the panel's map change only ever changed the
// running session, never the next boot.
//
// bootstrap (and the Go installer) create the file with de_dust2; the agent
// rewrites it whenever the panel changes the map, so a restart replays the map
// the operator actually chose.

// DefaultMapEnvPath is where bootstrap puts the game unit's EnvironmentFile.
// It matches CS2A_ROOT=/opt/cs2a.
const DefaultMapEnvPath = "/opt/cs2a/etc/cs2a-map"

// DefaultMap is the map a fresh install launches.
const DefaultMap = "de_dust2"

// mapEnvContent renders the EnvironmentFile for the game unit. A bare
// KEY=value line is the only syntax systemd's EnvironmentFile parser reads
// without surprises (no quotes, no shell expansion).
func mapEnvContent(mapName string) string {
	return fmt.Sprintf("CS2A_MAP=%s\n", mapName)
}

// WriteMapEnv records the map the next server start should load.
func WriteMapEnv(path, mapName string) error {
	if !reMapName.MatchString(mapName) {
		return fmt.Errorf("map env: invalid map name %q", mapName)
	}
	return atomicWrite(path, []byte(mapEnvContent(mapName)), 0o644)
}

// readMapEnv returns the recorded map, or "" when the file is absent or
// unreadable (fresh deploys, hand-rolled installs).
func readMapEnv(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	const p = "CS2A_MAP="
	if strings.HasPrefix(line, p) {
		return strings.TrimSpace(strings.TrimPrefix(line, p))
	}
	return ""
}

// EnsureMapEnv creates the EnvironmentFile if it is missing so the game unit
// can always start: systemd refuses to run a unit whose EnvironmentFile does
// not exist, and an empty CS2A_MAP would break the +map argument.
func EnsureMapEnv(path string) error {
	if _, err := os.ReadFile(path); err == nil {
		return nil
	}
	// The write is atomic (temp file + rename inside the same dir), so the
	// directory has to exist before it.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return WriteMapEnv(path, DefaultMap)
}
