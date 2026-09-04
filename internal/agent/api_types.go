package agent

import (
	"regexp"

	"cs2a/internal/cs2"
)

// cs2Setting is the JSON-friendly alias for a managed cvar setting.
type cs2Setting = cs2.CFGSetting

// reCvarName constrains cvar names written into the managed block.
var reCvarName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_\.]{1,63}$`)
