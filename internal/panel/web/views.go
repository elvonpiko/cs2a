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

// fmtFloat renders a bound without a trailing .0 (min="5" not min="5.000000").
func fmtFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// DownloadProgress fills the view's download fields from the job's live
// byte counters. Downloading is only true while the transfer is actually in
// flight (total known and bytes below it), so the bar appears for the phase
// that dominates an install and disappears for extraction/config steps.
func (v *PluginJobView) DownloadProgress(bytes, total int64) {
	if total <= 0 || bytes < 0 {
		// Chunked transfer: no total, so no meaningful bar — the step text
		// alone carries the phase.
		return
	}
	v.Downloading = true
	v.DownloadPct = int(bytes * 100 / total)
	if v.DownloadPct > 100 {
		v.DownloadPct = 100
	}
	v.DownloadLabel = fmtBytes(bytes) + " / " + fmtBytes(total)
}

// fmtBytes renders a byte count the way operators read download sizes.
func fmtBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KB"
	default:
		return strconv.FormatInt(n, 10) + " B"
	}
}

// ariaCurrent renders the aria-current attribute value for nav links.
func ariaCurrent(active bool) string {
	if active {
		return "page"
	}
	return "false"
}

// plural picks the singular or plural word for n (used inside templates).
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Plural is the exported form for handlers building user-facing strings with
// the same wording the templates use.
func Plural(n int, one, many string) string { return plural(n, one, many) }

// orDash renders an em dash for empty values so stat tiles never show a blank
// gap (an offline server has no map, hostname or uptime).
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ExitCodeLabel describes how the last run of the game binary ended, in
// operator terms rather than raw numbers: systemd reports "killed" + 11 for a
// segfault and "exited" + 1 for a clean non-zero exit.
func ExitCodeLabel(kind string, code int) string {
	switch {
	case kind == "killed" && code == 11:
		return "killed by signal 11 (segmentation fault)"
	case kind == "killed" && code > 0:
		return "killed by signal " + strconv.Itoa(code)
	case kind == "exited" && code > 0:
		return "exited with status " + strconv.Itoa(code)
	default:
		return ""
	}
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
		Plural(count, "SteamID", "SteamIDs") + " will be able to join; everyone else is disconnected."
}

// whitelistPlayerName names a whitelisted id in confirmations: the panel
// account when there is one, "this player" otherwise (an id added by hand for
// someone who never signed into the panel).
func whitelistPlayerName(name string) string {
	if name == "" {
		return "this player"
	}
	return name
}

// ServerView is the view model for the server page (both roles).
type ServerView struct {
	Online     bool
	ServiceSub string
	Hostname   string
	// ConnectAddr is the ip:port players use (agent config); empty means the
	// installer never worked out a public address.
	ConnectAddr  string
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
	// CrashLooping: the game binary dies during startup and systemd keeps
	// restarting it. The status card renders a hard problem card instead of
	// the usual running/offline hero, because "give it a minute" advice is
	// exactly wrong for this state.
	CrashLooping bool
	// RestartCount / ExitCodeLabel describe the loop ("restarted 5 times",
	// "killed by signal 11").
	RestartCount  int
	ExitCodeLabel string
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
	// Download progress (percent 0-100, and human-readable byte labels).
	// Downloading is true only while the artifact transfer is in flight.
	Downloading   bool
	DownloadPct   int
	DownloadLabel string
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
	Weapons     []WeaponOption
	// SkinT/SkinCT hold the current selection per weapon defindex ("7" →
	// "421"), mirroring the agent's Loadout.SkinsT/SkinsCT.
	SkinT  map[string]string
	SkinCT map[string]string
}

// WeaponOption is one weapon with its paint kits. Team is "T", "CT" or
// "both": the template shows one select per side, hiding the side a weapon
// cannot spawn with. NameT/NameCT are the ready-built form field names
// ("skin_t[7]") so the template stays string-concat free.
type WeaponOption struct {
	Defindex int
	Name     string
	Team     string
	Skins    []WeaponSkinOption
	NameT    string
	NameCT   string
	// DefindexLabel is the defindex as a string, for the pick helpers.
	DefindexLabel string
}

// WeaponSkinOption is one paint kit for one weapon (Value = paint id).
type WeaponSkinOption struct {
	Value string
	Label string
	Image string
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
// SettingsView renders the curated settings catalog (defined in
// settings_catalog.go, same package) with the current values filled in.
type SettingsView struct {
	Groups     []SettingGroup
	CFGWarning string
	// ExtraCount is how many managed rows exist outside the catalog; saves
	// preserve them, and the page says so instead of staying quiet.
	ExtraCount int
	// AgentDown replaces the form with an error when the settings could not
	// be read (saving blind would overwrite the block).
	AgentDown bool
}

type AccessView struct {
	// Password is only used as a boolean here (set / not set) — the value is
	// never rendered. It arrives from the agent's settings list; the password
	// card must not echo a secret back to whatever screen is open.
	Password string
	// WhitelistInstalled gates the whole whitelist card: the feature is a
	// plugin, and the card is a management surface for files that only exist
	// once it is installed. Before that, the only whitelist mention is one
	// line in the summary strip ("needs the CS2 Whitelist plugin") pointing
	// at the Plugins page; a greyed-out card that could not do anything was
	// read as "installed but broken".
	WhitelistInstalled bool
	WhitelistActive     bool
	WhitelistText       string
	WhitelistCount      int
	// WhitelistPlayers is the current list with per-entry panel-account names
	// resolved where a linked SteamID matches, so the card shows who is on it
	// rather than a bare textarea of ids.
	WhitelistPlayers []WhitelistPlayer
	// AddableUsers are panel users with a linked SteamID who are not yet on
	// the whitelist — the choices the "Add players" modal offers.
	AddableUsers []UserRow
	// CFGWarning explains a server.cfg cs2a can write to but not fully control
	// — a second managed block overrides everything shown here, so the page must
	// say so rather than presenting stale values as the truth.
	CFGWarning string
}

// WhitelistPlayer is one allowed SteamID as the card lists it.
type WhitelistPlayer struct {
	SteamID string
	// Name is the linked panel account's username, or "" when the id belongs
	// to no panel user (a friend added by id, never given a panel account).
	Name string
}

// AccessModeLabel names the effective access model for the summary strip:
// who can join the game server right now. Password and whitelist are
// independent layers — a passworded whitelisted server demands both.
func (v AccessView) AccessModeLabel() string {
	switch {
	case v.WhitelistActive && v.Password != "":
		return "Restrictive + password"
	case v.WhitelistActive:
		return "Restrictive only"
	case v.Password != "":
		return "Password only"
	default:
		return "Open to everyone"
	}
}

// AccessModeDetail is the one-line "who can join" answer for the current mode.
func (v AccessView) AccessModeDetail() string {
	switch {
	case v.WhitelistActive && v.Password != "":
		return "Only the listed SteamIDs can connect, and they must also know the password. Everyone else is rejected at the door."
	case v.WhitelistActive:
		return "Only the listed SteamIDs can connect — everyone else is rejected, password or not."
	case v.Password != "":
		return "Anyone who knows the password can connect; clients cache it after the first join."
	default:
		if v.WhitelistInstalled {
			return "Anyone on the internet can connect. Set a password or enforce the whitelist to keep it private."
		}
		return "Anyone on the internet can connect. Set a password to keep it private — closing it entirely needs the CS2 Whitelist plugin (Plugins page)."
	}
}

// PluginConfigView is the plugin config editor page model.
type PluginConfigView struct {
	ID     string
	Name   string
	JSON   string // pretty-printed current config
	Exists bool
	Note   string
}

// skinPick returns the selected paint id for one weapon ("" = none).
func skinPick(m map[string]string, defindex string) string {
	if m == nil {
		return ""
	}
	return m[defindex]
}

// weaponSkinsFor returns the weapons a side can carry: team "T" and "both"
// for the terrorist card, "CT" and "both" for the counter-terrorist one. A
// T-only gun never renders a CT picker, and vice versa.
func weaponSkinsFor(ws []WeaponOption, side string) []WeaponOption {
	out := make([]WeaponOption, 0, len(ws))
	for _, w := range ws {
		if w.Team == side || w.Team == "both" {
			out = append(out, w)
		}
	}
	return out
}

// labelPasswordCard names the input field without ever showing the value.
func labelPasswordCard(current string) string {
	if current == "" {
		return "Set a password"
	}
	return "Replace password"
}

// placeholderPasswordCard tells the operator what an empty save does, again
// without the value.
func placeholderPasswordCard(current string) string {
	if current == "" {
		return "empty = still no password"
	}
	return "empty = remove the password"
}

// submitLabelPasswordCard states the action the button performs.
func submitLabelPasswordCard(current string) string {
	if current == "" {
		return "Set password"
	}
	return "Replace password"
}
