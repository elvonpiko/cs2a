package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// patchGameinfoMetamod ensures `Game csgo/addons/metamod` is present in
// gameinfo.gi's SearchPaths (inserted right above the `Game csgo` line).
// Idempotent.
func patchGameinfoMetamod(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("gameinfo: read %s: %w", path, err)
	}
	content := string(raw)
	if strings.Contains(content, "csgo/addons/metamod") {
		return nil
	}
	// find the Game csgo line (tab or space separated)
	re := regexp.MustCompile(`(?m)^(\s*)Game(\s*)csgo(\s*)$`)
	m := re.FindStringSubmatchIndex(content)
	if m == nil {
		return fmt.Errorf("gameinfo: could not locate 'Game csgo' line in %s", path)
	}
	indent := content[m[2]:m[3]]
	line := fmt.Sprintf("%sGame\tcsgo/addons/metamod\n", indent)
	out := content[:m[0]] + line + content[m[0]:]
	if err := atomicWrite(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("gameinfo: write: %w", err)
	}
	return nil
}

// unpatchGameinfoMetamod removes the metamod SearchPath line again. Leaving it
// behind after uninstalling Metamod:Source makes the engine complain about a
// missing search path on every boot, and the panel offered no way to undo it.
// Idempotent, and a missing gameinfo.gi is not an error (the game may be gone).
func unpatchGameinfoMetamod(path string) error {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gameinfo: read %s: %w", path, err)
	}
	content := string(raw)
	if !strings.Contains(content, "csgo/addons/metamod") {
		return nil
	}
	// Drop only whole lines whose value is the metamod search path, so an
	// operator's own comment mentioning metamod survives.
	reLine := regexp.MustCompile(`(?m)^[ \t]*Game[ \t]+csgo/addons/metamod[ \t]*\r?\n`)
	out := reLine.ReplaceAllString(content, "")
	if out == content {
		return nil
	}
	if err := atomicWrite(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("gameinfo: write: %w", err)
	}
	return nil
}

// patchCoreGuidelines sets FollowCS2ServerGuidelines=false in cssharp's
// core.json (required for skin plugins to apply cosmetics). Creates a minimal
// file if cssharp has not generated one yet (it merges on next boot).
func patchCoreGuidelines(path string) error {
	var doc map[string]any
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &doc); err != nil {
			// keep a backup of the unparseable file rather than destroy it
			_ = os.WriteFile(path+".cs2a-bak", raw, 0o644)
			doc = map[string]any{}
		}
	} else {
		doc = map[string]any{}
	}
	doc["FollowCS2ServerGuidelines"] = false
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return err
	}
	return atomicWrite(path, buf.Bytes(), 0o644)
}

// dirOf is filepath.Dir with a name that reads better at call sites here.
func dirOf(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "."
	}
	return p[:i]
}
