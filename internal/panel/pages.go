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
	jobs := s.pluginJobViews(r)
	comp := web.Base("Plugins", navFor(u, "plugins"), web.PluginsPage(navFor(u, "plugins"), flash(r), entries, jobs))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render plugins", "err", err)
	}
}

// pluginJobViews lists in-flight and recently finished installs for the page's
// live progress strip.
func (s *Server) pluginJobViews(r *http.Request) []web.PluginJobView {
	jobs, err := s.agent.Jobs(r.Context())
	if err != nil {
		return nil
	}
	out := make([]web.PluginJobView, 0, len(jobs))
	for _, j := range jobs {
		name := j.Label
		if name == "" {
			name = j.Target
		}
		v := web.PluginJobView{
			ID:      j.ID,
			Name:    name,
			Status:  j.Status,
			Step:    j.Step,
			Message: j.Message,
			Running: j.Running(),
		}
		if j.Result != nil {
			v.Version = j.Result.Version
			v.RequiresRestart = j.Result.RequiresRestart
		}
		out = append(out, v)
	}
	return out
}

// handlePluginJobsPartial is polled by the plugins page while an install runs.
func (s *Server) handlePluginJobsPartial(w http.ResponseWriter, r *http.Request) {
	if err := web.PluginJobs(s.pluginJobViews(r)).Render(r.Context(), w); err != nil {
		s.log.Error("render plugin jobs", "err", err)
	}
}

func (s *Server) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	id := r.PathValue("id")
	// Installs run as a background job on the agent: downloads take minutes,
	// which no HTTP request (or reverse proxy) should be asked to hold open.
	job, err := s.agent.InstallAsync(r.Context(), id, false)
	if err != nil {
		redirectFlash(w, r, "/plugins", "err", "Install failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "plugin.install.start", id+" job="+job.ID)
	redirectFlash(w, r, "/plugins", "ok", "Installing "+id+" — progress appears below, you can leave this page.")
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
		}
	}
	// Enforcement lives in the whitelist plugin's own config, not in a cvar.
	if st, err := s.agent.WhitelistState(r.Context()); err == nil {
		v.WhitelistText = strings.Join(st.SteamIDs, "\n")
		v.WhitelistActive = st.Enabled
		v.WhitelistCount = len(st.SteamIDs)
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

// handleAccessWhitelistToggle switches whitelist enforcement on or off.
func (s *Server) handleAccessWhitelistToggle(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	on := r.FormValue("enabled") == "1"
	if err := s.agent.SetWhitelistEnabled(r.Context(), on); err != nil {
		redirectFlash(w, r, "/access", "err", "Could not change whitelist enforcement: "+err.Error())
		return
	}
	state := "disabled"
	if on {
		state = "enabled"
	}
	s.store.Audit(u.Username, "access.whitelist.enabled", state)
	redirectFlash(w, r, "/access", "ok", "Whitelist "+state+".")
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

// knifeCatalog is the WeaponPaints-compatible knife model list. Class names and
// labels are verified against the game's item schema: weapon_knife_outdoor is
// the Nomad Knife and weapon_knife_gypsy_jackknife is the Navaja Knife, which
// is easy to get backwards (both were previously mislabelled here, leaving two
// entries called "Skeleton Knife" and no Navaja).
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
	{Value: "weapon_knife_gypsy_jackknife", Label: "Navaja Knife"},
	{Value: "weapon_knife_outdoor", Label: "Nomad Knife"},
	{Value: "weapon_knife_stiletto", Label: "Stiletto Knife"},
	{Value: "weapon_knife_widowmaker", Label: "Talon Knife"},
	{Value: "weapon_knife_skeleton", Label: "Skeleton Knife"},
	{Value: "weapon_knife_kukri", Label: "Kukri Knife"},
}

func (s *Server) handleLoadoutPage(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	v := web.LoadoutView{SteamID: u.SteamID64, KnifeNames: knifeCatalog}
	// catalogs (gloves/agents) come from the agent; fall back to empty lists
	if gloves, agentsT, agentsCT, err := s.agent.Cosmetics(r.Context()); err == nil {
		for _, g := range gloves {
			v.Gloves = append(v.Gloves, web.GloveOption{Value: gloveValue(g.Defindex, g.Paint), Label: g.Name, Image: g.Image})
		}
		for _, a := range agentsT {
			v.AgentsT = append(v.AgentsT, web.AgentOption{Value: a.Model, Label: a.Name, Image: a.Image})
		}
		for _, a := range agentsCT {
			v.AgentsCT = append(v.AgentsCT, web.AgentOption{Value: a.Model, Label: a.Name, Image: a.Image})
		}
	}
	if u.SteamID64 != "" {
		lo, err := s.agent.GetLoadout(r.Context(), u.SteamID64)
		if err == nil {
			v.KnifeT = lo.KnifeT
			v.KnifeCT = lo.KnifeCT
			v.GlovesT = lo.GlovesT
			v.GlovesCT = lo.GlovesCT
			v.AgentT = lo.AgentT
			v.AgentCT = lo.AgentCT
			v.SyncEnabled = lo.SyncEnabled
		}
	}
	comp := web.Base("Loadout", navFor(u, "loadout"), web.LoadoutPage(navFor(u, "loadout"), flash(r), v))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render loadout", "err", err)
	}
}

// gloveValue encodes a glove as "<defindex>:<paint>" (default = "").
func gloveValue(defindex, paint int) string {
	if defindex == 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", defindex, paint)
}

func (s *Server) handleLoadoutPost(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	if u.SteamID64 == "" {
		redirectFlash(w, r, "/loadout", "err", "An admin must link a SteamID to your account first.")
		return
	}
	lo := &PlayerLoadout{
		KnifeT:   validKnife(r.FormValue("knife_t")),
		KnifeCT:  validKnife(r.FormValue("knife_ct")),
		GlovesT:  validGlove(r.FormValue("gloves_t")),
		GlovesCT: validGlove(r.FormValue("gloves_ct")),
		AgentT:   validAgent(r.FormValue("agent_t")),
		AgentCT:  validAgent(r.FormValue("agent_ct")),
	}
	if err := s.agent.PutLoadout(r.Context(), u.SteamID64, lo); err != nil {
		redirectFlash(w, r, "/loadout", "err", "Save failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "loadout.save", "t="+lo.KnifeT+" ct="+lo.KnifeCT+" gloves="+lo.GlovesT+"/"+lo.GlovesCT+" agents="+lo.AgentT+"/"+lo.AgentCT)
	redirectFlash(w, r, "/loadout", "ok", "Loadout saved — it applies when you (re)connect. Use !wp in game to force a refresh.")
}

// validGlove keeps "<defindex>:<paint>" or empty.
func validGlove(v string) string {
	if v == "" {
		return ""
	}
	var d, p int
	if _, err := fmt.Sscanf(v, "%d:%d", &d, &p); err != nil || d <= 0 {
		return ""
	}
	return v
}

// validAgent keeps model-path-looking strings (no spaces/quotes).
func validAgent(v string) string {
	if v == "" {
		return ""
	}
	for _, r := range v {
		if r == ' ' || r == '"' || r == '\'' {
			return ""
		}
	}
	return v
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
