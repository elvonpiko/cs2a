// Package web holds the cs2a panel templates (templ) and static assets.
package web

import "embed"

//go:embed static/*
var Static embed.FS

// NavUser is the minimal user context the layout needs.
type NavUser struct {
	Name    string
	Role    string // "admin" | "player"
	SteamID string
	Active  string // nav section key to highlight: server|plugins|access|users|loadout
	// ShowLoadout gates the Loadout tab: the page exists only when
	// WeaponPaints is installed AND this account has a linked SteamID.
	// Missing either, there is no loadout to manage — the tab is hidden
	// rather than leading to a page that nags for setup.
	ShowLoadout bool
}

// IsAdmin reports whether the nav user is an admin.
func (n *NavUser) IsAdmin() bool { return n != nil && n.Role == "admin" }
