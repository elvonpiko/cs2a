package web

import "strconv"

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
}

// PlayerRow is one online player line.
type PlayerRow struct {
	Name      string
	SteamID   string
	Connected string
	Ping      int
	State     string
	IsBot     bool
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
	Users           []UserRow
}

// PluginConfigView is the plugin config editor page model.
type PluginConfigView struct {
	ID     string
	Name   string
	JSON   string // pretty-printed current config
	Exists bool
	Note   string
}
