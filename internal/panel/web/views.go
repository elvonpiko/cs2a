package web

import "strconv"

// fmtInt renders an int for templ text nodes.
func fmtInt(n int) string { return strconv.Itoa(n) }

// fmtInt64 renders an int64 for templ attribute values.
func fmtInt64(n int64) string { return strconv.FormatInt(n, 10) }

// knifeLabel resolves a knife value to its display label.
func knifeLabel(value string, opts []KnifeOption) string {
	if value == "" {
		return "default"
	}
	for _, o := range opts {
		if o.Value == value {
			return o.Label
		}
	}
	return value
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
	SyncEnabled bool
	KnifeNames  []KnifeOption
}

// KnifeOption is one selectable knife.
type KnifeOption struct {
	Value string
	Label string
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
