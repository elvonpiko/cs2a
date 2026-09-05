package web

import (
	"strconv"
	"strings"
)

// mapImage resolves a map name to its bundled preview image ("" if unknown
// — the template hides the img in that case).
func mapImage(mapName string) string {
	if mapName == "" {
		return ""
	}
	return "/static/img/maps/" + mapName + ".jpg"
}

// fmtInt renders an int for templ text nodes.
func fmtInt(n int) string { return strconv.Itoa(n) }

// fmtInt64 renders an int64 for templ attribute values.
func fmtInt64(n int64) string { return strconv.FormatInt(n, 10) }

// ariaCurrent renders the aria-current attribute value for nav links.
func ariaCurrent(active bool) string {
	if active {
		return "page"
	}
	return "false"
}

// plural picks the singular or plural word for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// orDash renders an em dash for empty values so stat tiles never show a blank
// gap (an offline server has no map, hostname or uptime).
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// joinList renders a string slice as "a, b and c" for prose in templates.
func joinList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

// whitelistEnableWarning is the confirmation text for switching enforcement on.
// Everyone not on the list is disconnected the moment it takes effect, so the
// count is spelled out rather than left to "are you sure?".
func whitelistEnableWarning(count int) string {
	return "Enforce the whitelist? Only the " + strconv.Itoa(count) + " listed " +
		plural(count, "SteamID", "SteamIDs") + " will be able to join; everyone else is disconnected."
}

// ServerView is the view model for the server page (both roles).
type ServerView struct {
	Online       bool
	ServiceSub   string
	Hostname     string
	Map          string
	Players      int
	Max          int
	Bots         int
	UptimeLabel  string
	Note         string
	PlayerList   []PlayerRow
	Maps         []string
	CurrentMap   string
	IsAdmin      bool
	PanelVersion string
	// Problem explains an unreachable game server in plain words, replacing
	// the raw socket error the panel used to print.
	Problem string
	// ProblemFix is the one-line repair suggestion that accompanies Problem.
	ProblemFix string
	// CanRepair enables the "Fix it for me" button (the agent can apply the
	// change itself).
	CanRepair bool
	// LogLines is the tail of the game server's journal, shown when the server
	// is offline or misbehaving so the operator does not need SSH.
	LogLines []string
	// Polled marks a render that came from the 5 s status poll rather than a
	// full page load. Only then does StatusCard emit the out-of-band copies of
	// the lifecycle row and the player list; emitting them on a full render
	// would duplicate ids the page already contains.
	Polled bool
}

// PlayerRow is one online player line.
type PlayerRow struct {
	Name      string
	SteamID   string
	Addr      string
	Connected string
	Ping      int
	State     string
	IsBot     bool
}

// Label is the secondary line under a player's name. CS2's status table carries
// no SteamID at all, so the row shows whatever identifying detail exists rather
// than an empty separator string.
func (p PlayerRow) Label() string {
	var parts []string
	if p.SteamID != "" {
		parts = append(parts, p.SteamID)
	} else if p.Addr != "" {
		parts = append(parts, p.Addr)
	}
	if p.Connected != "" {
		parts = append(parts, p.Connected)
	}
	if p.Ping > 0 {
		parts = append(parts, fmtInt(p.Ping)+" ms")
	}
	if len(parts) == 0 {
		if p.IsBot {
			return "bot"
		}
		return p.State
	}
	return strings.Join(parts, " · ")
}

// Toast is an inline user-facing message (POST-redirect-GET flash).
type Toast struct {
	Kind    string // ok | err
	Message string
}

// PluginCardView is one catalog entry for the plugins page.
type PluginCardView struct {
	ID          string
	Name        string
	Description string
	Author      string
	Kind        string
	Homepage    string
	Installed   bool
	Version     string
	HasConfig   bool
	Requires    []string
	// RequiredBy names installed plugins that depend on this one. While it is
	// non-empty the card explains that instead of offering Uninstall, which
	// used to remove Metamod out from under everything else.
	RequiredBy []string
}

// PluginJobView is one in-flight or recently finished install.
type PluginJobView struct {
	ID              string
	Name            string
	Status          string // running | done | failed
	Step            string
	Message         string
	Version         string
	Running         bool
	RequiresRestart bool
	// Warning is a non-fatal problem reported by a successful install.
	Warning string
}

// AnyRunning reports whether at least one job is still working (drives polling).
func AnyRunning(jobs []PluginJobView) bool {
	for _, j := range jobs {
		if j.Running {
			return true
		}
	}
	return false
}

// UsersView is the admin users page model.
type UsersView struct {
	Users []UserRow
}

// UserRow is one account row.
type UserRow struct {
	ID       int64
	Username string
	Role     string
	SteamID  string
	Created  string
}

// LoadoutView is the player loadout page model.
type LoadoutView struct {
	SteamID     string
	KnifeT      string
	KnifeCT     string
	GlovesT     string
	GlovesCT    string
	AgentT      string
	AgentCT     string
	SyncEnabled bool
	KnifeNames  []KnifeOption
	Gloves      []GloveOption
	AgentsT     []AgentOption
	AgentsCT    []AgentOption
}

// KnifeOption is one selectable knife.
type KnifeOption struct {
	Value string
	Label string
}

// GloveOption is one selectable glove pair (Value = "<defindex>:<paint>").
type GloveOption struct {
	Value string
	Label string
	Image string
}

// AgentOption is one selectable agent model (Value = model path).
type AgentOption struct {
	Value string
	Label string
	Image string
}

// AccessView is the admin access page model.
type AccessView struct {
	Password        string
	WhitelistActive bool
	WhitelistText   string
	WhitelistCount  int
	Users           []UserRow
	// CFGWarning explains a server.cfg cs2a can write to but not fully control
	// — a second managed block overrides everything shown here, so the page must
	// say so rather than presenting stale values as the truth.
	CFGWarning string
}

// PluginConfigView is the plugin config editor page model.
type PluginConfigView struct {
	ID     string
	Name   string
	JSON   string // pretty-printed current config
	Exists bool
	Note   string
}
