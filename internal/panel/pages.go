package panel

import (
	"fmt"
	"net/http"
	"strings"

	"cs2a/internal/cs2"
	"cs2a/internal/panel/web"
)

// uptimeLabel renders seconds as "3d 4h", "2h 15m", "45s".
func uptimeLabel(secs float64) string {
	if secs <= 0 {
		return "—"
	}
	d := int(secs) / 86400
	h := (int(secs) % 86400) / 3600
	m := (int(secs) % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// buildServerView assembles the server page model from the agent.
func (s *Server) buildServerView(ctx *http.Request, u *User) web.ServerView {
	v := web.ServerView{IsAdmin: u.Role == "admin", PanelVersion: panelVersion}
	st, err := s.agent.Status(ctx.Context())
	if err != nil {
		v.Note = "agent unreachable: " + err.Error()
		return v
	}
	v.Online = st.Service.Active
	v.ServiceSub = "systemd unit state: " + boolLabel(st.Service.Active)
	if st.Info != nil {
		v.Hostname = st.Info.Name
		v.Map = st.Info.Map
		v.Players = st.Info.Players
		v.Max = st.Info.Max
		v.Bots = st.Info.Bots
	}
	if st.Rcon != nil {
		if v.Map == "" {
			v.Map = st.Rcon.Map
		}
		v.PlayerList = make([]web.PlayerRow, 0, len(st.Rcon.Players))
		for _, p := range st.Rcon.Players {
			v.PlayerList = append(v.PlayerList, web.PlayerRow{
				Name:      p.Name,
				SteamID:   p.SteamID,
				Connected: p.Connected,
				Ping:      p.Ping,
				State:     p.State,
				IsBot:     p.SteamID == "",
			})
		}
	}
	if st.Service.UptimeSeconds > 0 {
		v.UptimeLabel = uptimeLabel(st.Service.UptimeSeconds)
	}
	if v.Note == "" {
		v.Note = st.Note
	}
	if maps, err := s.agent.Maps(ctx.Context()); err == nil {
		v.Maps = maps
		v.CurrentMap = v.Map
	}
	return v
}

func boolLabel(b bool) string {
	if b {
		return "running"
	}
	return "stopped"
}

// --- pages ---------------------------------------------------------------

func (s *Server) handleServerPage(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	v := s.buildServerView(r, u)
	comp := web.Base("Server", navFor(u, "server"), web.ServerPage(navFor(u, "server"), flash(r), v))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render server page", "err", err)
	}
}

func (s *Server) handleStatusCardPartial(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	v := s.buildServerView(r, u)
	if err := web.StatusCard(v).Render(r.Context(), w); err != nil {
		s.log.Error("render status partial", "err", err)
	}
}

// --- server actions -------------------------------------------------------

func (s *Server) handleServerAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFromCtx(r)
		if err := s.agent.ServerAction(r.Context(), action); err != nil {
			redirectFlash(w, r, "/", "err", actionError(action, err))
			return
		}
		s.store.Audit(u.Username, "server."+action, "")
		redirectFlash(w, r, "/", "ok", actionMessage(action))
	}
}

func actionMessage(action string) string {
	switch action {
	case "start":
		return "Server starting — it may take a minute to appear online."
	case "stop":
		return "Server stopped."
	default:
		return "Server restarting — status will update live."
	}
}

func actionError(action string, err error) string {
	return "Failed to " + action + " server: " + err.Error()
}

func (s *Server) handleMapChange(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	mapName := strings.TrimSpace(r.FormValue("map"))
	if mapName == "" {
		redirectFlash(w, r, "/", "err", "Pick a map first.")
		return
	}
	if err := s.agent.ChangeMap(r.Context(), mapName, false); err != nil {
		redirectFlash(w, r, "/", "err", "Map change failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "map.change", mapName)
	redirectFlash(w, r, "/", "ok", "Changing map to "+mapName+"…")
}

// --- plugins ---------------------------------------------------------------

func (s *Server) pluginCatalogCards(w http.ResponseWriter, r *http.Request) []web.PluginCardView {
	entries, err := s.agent.Plugins(r.Context())
	if err != nil {
		redirectFlash(w, r, "/", "err", "Agent unreachable: "+err.Error())
		return nil
	}
	out := make([]web.PluginCardView, 0, len(entries))
	for _, e := range entries {
		v := web.PluginCardView{
			ID:        e.ID,
			Name:      e.Name,
			Author:    e.Author,
			Kind:      e.Kind,
			Homepage:  e.Homepage,
			HasConfig: true, // presence is resolved by the config page
			Requires:  e.Requires,
		}
		v.Description = e.Description
		installed, version := parseInstalledAnnotation(e.Description)
		v.Installed = installed
		v.Version = version
		if v.Installed {
			// strip the annotation from the description for display
			v.Description = stripInstalledAnnotation(e.Description)
		}
		out = append(out, v)
	}
	return out
}

func parseInstalledAnnotation(desc string) (bool, string) {
	if !strings.HasPrefix(desc, "[installed ") {
		return false, ""
	}
	end := strings.Index(desc, "]")
	if end < 0 {
		return false, ""
	}
	return true, desc[len("[installed "):end]
}

func stripInstalledAnnotation(desc string) string {
	if !strings.HasPrefix(desc, "[installed ") {
		return desc
	}
	if end := strings.Index(desc, "]"); end >= 0 {
		return strings.TrimSpace(desc[end+1:])
	}
	return desc
}

func (s *Server) handlePluginsPage(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	entries := s.pluginCatalogCards(w, r)
	if entries == nil {
		return // redirected with flash
	}
	comp := web.Base("Plugins", navFor(u, "plugins"), web.PluginsPage(navFor(u, "plugins"), flash(r), entries))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render plugins", "err", err)
	}
}

func (s *Server) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	id := r.PathValue("id")
	res, err := s.agent.Install(r.Context(), id, false)
	if err != nil {
		redirectFlash(w, r, "/plugins", "err", "Install failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "plugin.install", id+"@"+res.Version)
	msg := "Installed " + id + " " + res.Version
	if res.InstalledDeps {
		msg += " (with dependencies)"
	}
	if res.RequiresRestart {
		msg += ". A server restart is required to load it."
	}
	redirectFlash(w, r, "/plugins", "ok", msg)
}

func (s *Server) handlePluginUninstall(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	id := r.PathValue("id")
	if err := s.agent.Uninstall(r.Context(), id); err != nil {
		redirectFlash(w, r, "/plugins", "err", "Uninstall failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "plugin.uninstall", id)
	redirectFlash(w, r, "/plugins", "ok", "Uninstalled "+id+". A restart is recommended.")
}

func (s *Server) handlePluginConfigPage(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	id := r.PathValue("id")
	raw, err := s.agent.PluginConfig(r.Context(), id)
	if err != nil {
		redirectFlash(w, r, "/plugins", "err", err.Error())
		return
	}
	v := web.PluginConfigView{ID: id, Name: id, Exists: raw != nil}
	v.JSON = strings.TrimSpace(string(raw))
	if v.JSON == "" {
		v.JSON = "{\n  \n}"
	}
	comp := web.Base("Config", navFor(u, "plugins"), web.PluginConfigPage(navFor(u, "plugins"), flash(r), v))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render plugin config", "err", err)
	}
}

func (s *Server) handlePluginConfigPost(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	id := r.PathValue("id")
	body := r.FormValue("json")
	if err := s.agent.SavePluginConfig(r.Context(), id, body); err != nil {
		flash := web.Toast{Kind: "err", Message: "Save failed: " + err.Error()}
		v := web.PluginConfigView{ID: id, Name: id, JSON: body}
		comp := web.Base("Config", navFor(u, "plugins"), web.PluginConfigPage(navFor(u, "plugins"), &flash, v))
		_ = comp.Render(r.Context(), w)
		return
	}
	s.store.Audit(u.Username, "plugin.config", id)
	redirectFlash(w, r, "/plugins/"+id+"/config", "ok", "Config saved. Some plugins need a restart to pick it up.")
}

// --- access -----------------------------------------------------------------

func (s *Server) handleAccessPage(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	v := web.AccessView{}

	if settings, err := s.agent.Settings(r.Context()); err == nil {
		for _, set := range settings {
			if set.Name == "sv_password" && set.Value != "0" && set.Value != "" {
				v.Password = set.Value
			}
			if set.Name == "mm_whitelist_enable" {
				v.WhitelistActive = set.Value == "1"
			}
		}
	}
	if ids, err := s.agent.Whitelist(r.Context()); err == nil {
		v.WhitelistText = strings.Join(ids, "\n")
		v.WhitelistActive = v.WhitelistActive && len(ids) > 0
	}
	if users, err := s.store.ListUsers(); err == nil {
		for _, uu := range users {
			v.Users = append(v.Users, web.UserRow{
				ID: uu.ID, Username: uu.Username, Role: uu.Role, SteamID: uu.SteamID64,
				Created: uu.CreatedAt.Format("2006-01-02"),
			})
		}
	}
	comp := web.Base("Access", navFor(u, "access"), web.AccessPage(navFor(u, "access"), flash(r), v))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render access", "err", err)
	}
}

func (s *Server) handleAccessPassword(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	pw := strings.TrimSpace(r.FormValue("password"))
	if len(pw) > 128 {
		redirectFlash(w, r, "/access", "err", "Password too long.")
		return
	}
	if err := s.agent.SetPassword(r.Context(), pw); err != nil {
		redirectFlash(w, r, "/access", "err", "Could not set password: "+err.Error())
		return
	}
	detail := "set"
	if pw == "" {
		detail = "cleared"
	}
	s.store.Audit(u.Username, "access.password", detail)
	redirectFlash(w, r, "/access", "ok", "Server password "+detail+" — applies live, no restart needed.")
}

func (s *Server) handleAccessWhitelist(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	raw := r.FormValue("steamids")
	var ids []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			ids = append(ids, line)
		}
	}
	if err := s.agent.PutWhitelist(r.Context(), ids); err != nil {
		redirectFlash(w, r, "/access", "err", "Whitelist save failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "access.whitelist", fmt.Sprintf("%d entries", len(ids)))
	redirectFlash(w, r, "/access", "ok", "Whitelist saved (normalized to SteamID64).")
}

func (s *Server) handleAccessWhitelistAddUser(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	idStr := r.FormValue("user_id")
	var userID int64
	if _, err := fmt.Sscanf(idStr, "%d", &userID); err != nil {
		redirectFlash(w, r, "/access", "err", "Invalid user id.")
		return
	}
	target, err := s.store.GetUserByID(userID)
	if err != nil || target.SteamID64 == "" {
		redirectFlash(w, r, "/access", "err", "User has no linked SteamID.")
		return
	}
	ids, err := s.agent.Whitelist(r.Context())
	if err != nil {
		redirectFlash(w, r, "/access", "err", "Agent unreachable: "+err.Error())
		return
	}
	for _, id := range ids {
		if id == target.SteamID64 {
			redirectFlash(w, r, "/access", "ok", target.Username+" is already whitelisted.")
			return
		}
	}
	ids = append(ids, target.SteamID64)
	if err := s.agent.PutWhitelist(r.Context(), ids); err != nil {
		redirectFlash(w, r, "/access", "err", "Whitelist save failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "access.whitelist.add", target.Username+" "+target.SteamID64)
	redirectFlash(w, r, "/access", "ok", target.Username+" added to whitelist.")
}

// --- users -------------------------------------------------------------------

func (s *Server) handleUsersPage(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	v := web.UsersView{}
	if users, err := s.store.ListUsers(); err == nil {
		for _, uu := range users {
			v.Users = append(v.Users, web.UserRow{
				ID: uu.ID, Username: uu.Username, Role: uu.Role, SteamID: uu.SteamID64,
				Created: uu.CreatedAt.Format("2006-01-02"),
			})
		}
	}
	comp := web.Base("Users", navFor(u, "users"), web.UsersPage(navFor(u, "users"), flash(r), v, u.ID))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render users", "err", err)
	}
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	role := r.FormValue("role")
	if role != "admin" && role != "player" {
		role = "player"
	}
	hash, err := HashPassword(password)
	if err != nil {
		redirectFlash(w, r, "/users", "err", err.Error())
		return
	}
	steamID, steamErr := cs2Normalize(r.FormValue("steamid"))
	if steamErr != nil {
		redirectFlash(w, r, "/users", "err", "Invalid SteamID: "+steamErr.Error())
		return
	}
	if _, err := s.store.CreateUser(username, hash, role, steamID); err != nil {
		redirectFlash(w, r, "/users", "err", "Create failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "user.create", username+" ("+role+")")
	redirectFlash(w, r, "/users", "ok", "User "+username+" created.")
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	var userID int64
	if _, err := fmt.Sscanf(r.FormValue("user_id"), "%d", &userID); err != nil {
		redirectFlash(w, r, "/users", "err", "Invalid user id.")
		return
	}
	if userID == u.ID {
		redirectFlash(w, r, "/users", "err", "You cannot delete your own account.")
		return
	}
	target, err := s.store.GetUserByID(userID)
	if err != nil {
		redirectFlash(w, r, "/users", "err", "User not found.")
		return
	}
	if err := s.store.DeleteUser(userID); err != nil {
		redirectFlash(w, r, "/users", "err", "Delete failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "user.delete", target.Username)
	redirectFlash(w, r, "/users", "ok", "User "+target.Username+" deleted.")
}

// --- loadout -------------------------------------------------------------------

// knifeCatalog is the WeaponPaints-compatible knife model list (model names
// verified against the plugin's defindex map).
var knifeCatalog = []web.KnifeOption{
	{Value: "default", Label: "Default knife"},
	{Value: "weapon_bayonet", Label: "Bayonet"},
	{Value: "weapon_knife_css", Label: "Classic Knife"},
	{Value: "weapon_knife_flip", Label: "Flip Knife"},
	{Value: "weapon_knife_gut", Label: "Gut Knife"},
	{Value: "weapon_knife_karambit", Label: "Karambit"},
	{Value: "weapon_knife_m9_bayonet", Label: "M9 Bayonet"},
	{Value: "weapon_knife_tactical", Label: "Huntsman Knife"},
	{Value: "weapon_knife_falchion", Label: "Falchion Knife"},
	{Value: "weapon_knife_survival_bowie", Label: "Bowie Knife"},
	{Value: "weapon_knife_butterfly", Label: "Butterfly Knife"},
	{Value: "weapon_knife_push", Label: "Shadow Daggers"},
	{Value: "weapon_knife_cord", Label: "Paracord Knife"},
	{Value: "weapon_knife_canis", Label: "Survival Knife"},
	{Value: "weapon_knife_ursus", Label: "Ursus Knife"},
	{Value: "weapon_knife_gypsy_jackknife", Label: "Nomad Knife"},
	{Value: "weapon_knife_outdoor", Label: "Skeleton Knife"},
	{Value: "weapon_knife_stiletto", Label: "Stiletto Knife"},
	{Value: "weapon_knife_widowmaker", Label: "Talon Knife"},
	{Value: "weapon_knife_skeleton", Label: "Skeleton Knife"},
	{Value: "weapon_knife_kukri", Label: "Kukri Knife"},
}

func (s *Server) handleLoadoutPage(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	v := web.LoadoutView{SteamID: u.SteamID64, KnifeNames: knifeCatalog}
	if u.SteamID64 != "" {
		lo, err := s.agent.GetLoadout(r.Context(), u.SteamID64)
		if err != nil {
			// agent may be down; show the page with defaults
			v.SteamID = u.SteamID64
		} else {
			v.KnifeT = lo.KnifeT
			v.KnifeCT = lo.KnifeCT
			v.SyncEnabled = lo.SyncEnabled
		}
	}
	comp := web.Base("Loadout", navFor(u, "loadout"), web.LoadoutPage(navFor(u, "loadout"), flash(r), v))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render loadout", "err", err)
	}
}

func (s *Server) handleLoadoutPost(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	if u.SteamID64 == "" {
		redirectFlash(w, r, "/loadout", "err", "An admin must link a SteamID to your account first.")
		return
	}
	knifeT := validKnife(r.FormValue("knife_t"))
	knifeCT := validKnife(r.FormValue("knife_ct"))
	if err := s.agent.PutLoadout(r.Context(), u.SteamID64, knifeT, knifeCT); err != nil {
		redirectFlash(w, r, "/loadout", "err", "Save failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "loadout.save", "t="+knifeT+" ct="+knifeCT)
	redirectFlash(w, r, "/loadout", "ok", "Loadout saved — it applies when you (re)connect. Use !wp in game to force a refresh.")
}

func validKnife(v string) string {
	for _, k := range knifeCatalog {
		if k.Value == v {
			return v
		}
	}
	return "default"
}

// cs2Normalize validates a user-supplied SteamID, returning "" when empty.
func cs2Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	return cs2.NormalizeSteamID(raw)
}
