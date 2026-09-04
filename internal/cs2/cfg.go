package cs2

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ManagedBlockBegin / ManagedBlockEnd delimit the region of server.cfg that
// cs2a owns. Everything outside the block is user-owned and is preserved
// byte-for-byte on every edit.
const (
	ManagedBlockBegin = "// >>> cs2a managed block (do not edit between these markers) >>>"
	ManagedBlockEnd   = "// <<< cs2a managed block <<<"
)

// CFGSetting is one cvar assignment inside the managed block ("name value").
type CFGSetting struct {
	Name    string
	Value   string
	Comment string // optional; rendered as "// comment" above the line
}

// RenderManagedBlock renders the managed block content with markers.
func RenderManagedBlock(settings []CFGSetting) string {
	var b strings.Builder
	b.WriteString(ManagedBlockBegin + "\n")
	for _, s := range settings {
		if s.Comment != "" {
			b.WriteString("// " + s.Comment + "\n")
		}
		b.WriteString(s.Name + " \"" + sanitizeValue(s.Value) + "\"\n")
	}
	b.WriteString(ManagedBlockEnd)
	return b.String()
}

// sanitizeValue strips quotes and newlines so a value can never break out of
// its quoted cvar argument.
func sanitizeValue(v string) string {
	v = strings.ReplaceAll(v, "\"", "")
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.TrimSpace(v)
}

var reManagedBlock = regexp.MustCompile(
	regexp.QuoteMeta(ManagedBlockBegin) + `[\s\S]*?` + regexp.QuoteMeta(ManagedBlockEnd))

// ApplyManagedBlock replaces (or appends, if absent) the cs2a managed block in
// the given server.cfg content.
func ApplyManagedBlock(content string, settings []CFGSetting) string {
	block := RenderManagedBlock(settings)
	if reManagedBlock.MatchString(content) {
		return reManagedBlock.ReplaceAllLiteralString(content, block)
	}
	out := strings.TrimRight(content, "\n")
	if out != "" {
		out += "\n\n"
	}
	return out + block + "\n"
}

// ExtractManagedBlock returns the settings currently inside the managed block.
// Settings outside the block are not reported (they belong to the user).
func ExtractManagedBlock(content string) []CFGSetting {
	m := reManagedBlock.FindString(content)
	if m == "" {
		return nil
	}
	var out []CFGSetting
	var comment string
	for _, line := range strings.Split(m, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == ManagedBlockBegin || line == ManagedBlockEnd:
			comment = ""
		case strings.HasPrefix(line, "//"):
			comment = strings.TrimSpace(strings.TrimPrefix(line, "//"))
		case line != "":
			fields := strings.SplitN(line, " ", 2)
			if len(fields) == 2 {
				val := strings.Trim(fields[1], `"`)
				out = append(out, CFGSetting{Name: fields[0], Value: val, Comment: comment})
			}
			comment = ""
		}
	}
	return out
}

// LoadServerCFG reads the server.cfg at dir.
func LoadServerCFG(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "server.cfg"))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SaveServerCFG atomically writes server.cfg (write temp + rename).
func SaveServerCFG(dir, content string) error {
	target := filepath.Join(dir, "server.cfg")
	tmp, err := os.CreateTemp(dir, ".server.cfg-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}
