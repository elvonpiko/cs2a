package web

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
