package cs2

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cs2a/internal/fsatomic"
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
	Name    string `json:"name"`
	Value   string `json:"value"`
	Comment string `json:"comment,omitempty"`
	// Bare marks a line that carries no value, such as a plain `mp_autokick`
	// an operator added inside the block. Rendering it as `mp_autokick ""`
	// would set the cvar to 0 — and push that live over RCON — so the line is
	// round-tripped exactly as it was found.
	Bare bool `json:"bare,omitempty"`
}

// RenderManagedBlock renders the managed block content with markers.
func RenderManagedBlock(settings []CFGSetting) string {
	var b strings.Builder
	b.WriteString(ManagedBlockBegin + "\n")
	for _, s := range settings {
		if c := sanitizeComment(s.Comment); c != "" {
			b.WriteString("// " + c + "\n")
		}
		if s.Bare {
			b.WriteString(s.Name + "\n")
			continue
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

// sanitizeComment folds a comment onto one line. A newline here used to inject a
// live config line: a comment of "note\nrcon_password \"leaked\"" rendered
// rcon_password as its own executable statement inside the managed block, which
// the engine then ran on every boot.
func sanitizeComment(c string) string {
	c = strings.ReplaceAll(c, "\r", " ")
	c = strings.ReplaceAll(c, "\n", " ")
	return strings.TrimSpace(c)
}

// CvarCommand builds the console command that assigns value to name, using the
// same sanitizing as the file so the live value and the saved value cannot
// diverge.
//
// %q was wrong here: it escapes with backslashes, which the Source console does
// not interpret, so a value containing a quote was pushed live as a different
// string than the one written to server.cfg — and the engine parsed the rest of
// the line as further arguments.
func CvarCommand(name, value string) string {
	return name + " \"" + sanitizeValue(value) + "\""
}

// findManagedBlock locates the cs2a block in content and returns the byte range
// covering it, markers included and the trailing newline excluded.
//
// Markers are matched as whole lines, and the pair chosen is the smallest one:
// the end marker is the first in the file, and the begin marker is the LAST one
// before it. That matters because a substring search across the whole file
// destroyed hand-written config. If an operator deletes or annotates the end
// marker, the next write appends a second block, and a first-BEGIN-to-first-END
// match would then span everything the operator wrote between the two blocks and
// delete it. Taking the nearest pair means cs2a only ever rewrites a region
// delimited by markers it can see.
//
// An end marker with no begin marker before it is not a block: the orphan is
// left alone rather than adopted.
func findManagedBlock(content string) (start, end int, ok bool) {
	lastBegin := -1
	for off := 0; off <= len(content); {
		nl := strings.IndexByte(content[off:], '\n')
		lineEnd := len(content)
		next := len(content) + 1
		if nl >= 0 {
			lineEnd = off + nl
			next = lineEnd + 1
		}
		line := strings.TrimSpace(strings.TrimSuffix(content[off:lineEnd], "\r"))
		switch line {
		case ManagedBlockBegin:
			lastBegin = off
		case ManagedBlockEnd:
			if lastBegin >= 0 {
				// Stop before the line terminator so a CRLF file keeps its
				// \r\n: it belongs to the surrounding file, not to the block.
				if lineEnd > off && content[lineEnd-1] == '\r' {
					lineEnd--
				}
				return lastBegin, lineEnd, true
			}
		}
		off = next
	}
	return 0, 0, false
}

// CountManagedBlocks reports how many complete marker pairs the content
// contains.
//
// More than one is a real hazard rather than a curiosity: cs2a edits the first
// block, the engine executes both in order, so the second block's values are
// what the server actually runs. The panel would then show settings the server
// is not using, with no visible reason. cs2a will not silently rewrite a region
// the operator cannot see in the UI, so the situation is reported instead.
func CountManagedBlocks(content string) int {
	n := 0
	for {
		_, end, ok := findManagedBlock(content)
		if !ok {
			return n
		}
		n++
		if end >= len(content) {
			return n
		}
		content = content[end:]
	}
}

// ManagedBlockConflict describes a server.cfg with more than one managed block.
// It is returned as a warning, not an error: the save itself succeeded.
func ManagedBlockConflict(content string) string {
	if n := CountManagedBlocks(content); n > 1 {
		return fmt.Sprintf("server.cfg contains %d cs2a blocks; the last one wins in-game, "+
			"so settings shown here may not be the ones the server uses — delete the extra block", n)
	}
	return ""
}

// ApplyManagedBlock replaces (or appends, if absent) the cs2a managed block in
// the given server.cfg content.
//
// Only the first block is replaced; a duplicate block (a copied server.cfg) is
// left alone rather than kept in lockstep, because rewriting a block the UI
// cannot show would let the panel and the running server disagree silently.
// Callers should surface ManagedBlockConflict so the operator learns about it.
// Line endings are preserved: a CRLF file keeps CRLF.
func ApplyManagedBlock(content string, settings []CFGSetting) string {
	crlf := usesCRLF(content)
	block := RenderManagedBlock(settings)
	if crlf {
		block = strings.ReplaceAll(block, "\n", "\r\n")
	}
	if start, end, ok := findManagedBlock(content); ok {
		return content[:start] + block + content[end:]
	}
	nl := "\n"
	if crlf {
		nl = "\r\n"
	}
	out := strings.TrimRight(content, "\r\n")
	if out != "" {
		out += nl + nl
	}
	return out + block + nl
}

// usesCRLF reports whether content is CRLF-terminated. Mixing endings in one
// file makes every backup and diff of server.cfg churn.
func usesCRLF(content string) bool {
	i := strings.IndexByte(content, '\n')
	return i > 0 && content[i-1] == '\r'
}

// ExtractManagedBlock returns the settings currently inside the managed block.
// Settings outside the block are not reported (they belong to the user).
//
// Parsing is deliberately forgiving about spacing: a tab-separated line, extra
// spaces, and a trailing comment all used to be mangled or dropped, and the next
// write then persisted the damage — changing the server password could delete
// unrelated managed cvars, or push a value with a comment glued to it live over
// RCON.
func ExtractManagedBlock(content string) []CFGSetting {
	start, end, ok := findManagedBlock(content)
	if !ok {
		return nil
	}
	m := content[start:end]
	var out []CFGSetting
	var comment string
	for _, line := range strings.Split(m, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		switch {
		case line == ManagedBlockBegin || line == ManagedBlockEnd:
			comment = ""
		case strings.HasPrefix(line, "//"):
			comment = strings.TrimSpace(strings.TrimPrefix(line, "//"))
		case line != "":
			set := splitCvarLine(line)
			set.Comment = comment
			out = append(out, set)
			comment = ""
		}
	}
	return out
}

// splitCvarLine parses one `name value` config line the way the engine does:
// any run of whitespace separates them, a quoted value keeps its inner spaces,
// and a trailing // comment is not part of the value.
//
// A line with no value at all is a *query* — the engine prints the cvar's
// current value — so it is reported as Bare and rendered back unchanged.
// Dropping it deleted the operator's line on the next write; turning it into
// `mp_autokick ""` silently set the cvar to 0 and pushed that live over RCON.
func splitCvarLine(line string) CFGSetting {
	i := strings.IndexFunc(line, isCfgSpace)
	if i < 0 {
		return CFGSetting{Name: line, Bare: true}
	}
	name := line[:i]
	rest := strings.TrimLeft(line[i:], " \t")
	if strings.HasPrefix(rest, `"`) {
		// Quoted: the value ends at the closing quote, so a // inside it is
		// data, not a comment.
		if end := strings.IndexByte(rest[1:], '"'); end >= 0 {
			return CFGSetting{Name: name, Value: rest[1 : 1+end]}
		}
		return CFGSetting{Name: name, Value: strings.TrimSpace(rest[1:])}
	}
	if c := strings.Index(rest, "//"); c >= 0 {
		rest = rest[:c]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		// `mp_autokick   // note` is still a query, not an assignment.
		return CFGSetting{Name: name, Bare: true}
	}
	return CFGSetting{Name: name, Value: rest}
}

func isCfgSpace(r rune) bool { return r == ' ' || r == '\t' }

// LoadServerCFG reads the server.cfg at dir.
func LoadServerCFG(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "server.cfg"))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SaveServerCFG atomically writes server.cfg, keeping the mode and owner the
// operator's file already has.
//
// This used to be a plain CreateTemp + Rename, which replaced the file with a
// root-owned 0600 inode — the game server runs unprivileged, so it could no
// longer read its own config. Every managed cvar, rcon_password included,
// silently stopped being applied at the next map load, and the panel reported
// success. A brand new file is created 0644 so the game can read it.
func SaveServerCFG(dir, content string) error {
	return fsatomic.WriteKeepMode(filepath.Join(dir, "server.cfg"), []byte(content), 0o644)
}
