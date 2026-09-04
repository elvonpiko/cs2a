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
}

// IsAdmin reports whether the nav user is an admin.
func (n *NavUser) IsAdmin() bool { return n != nil && n.Role == "admin" }

// Version is shown in the footer; set from the panel binary.
var Version = "dev"
