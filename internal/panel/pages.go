package panel

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"

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

// buildServerView assembles the full server page model from the agent.
func (s *Server) buildServerView(r *http.Request, u *User) web.ServerView {
	return s.serverView(r, u, false)
}

// serverView builds the server page model. polled trims the work to what the
// 5 s status refresh actually swaps: the map list means a directory scan and the
// journal tail means spawning journalctl, and neither is part of the polled
// regions — doing them anyway cost two extra agent round-trips every five
// seconds for every open admin tab.
func (s *Server) serverView(r *http.Request, u *User, polled bool) web.ServerView {
	ctx := r.Context()
	v := web.ServerView{IsAdmin: u.Role == "admin", PanelVersion: panelVersion, Polled: polled}
	st, err := s.agent.Status(ctx)
	if err != nil {
		v.Problem = "The panel cannot reach the cs2a agent."
		v.ProblemFix = err.Error()
		return v
	}
	v.Online = st.Service.Active
	v.ServiceSub = "systemd unit state: " + boolLabel(st.Service.Active)
	v.ConnectAddr = st.ConnectAddr
	// A crash-looping unit must not be rendered as "Running": is-active flips
	// on and off every few seconds, which is exactly what the operator cannot
	// diagnose from the page. The agent already zeroed Active and folded the
	// journal tail into the status card; this view adds the operator wording.
	if st.Service.CrashLooping {
		v.Online = false
		v.CrashLooping = true
		v.RestartCount = st.Service.RestartCount
		v.ExitCodeLabel = exitStatusLabel(st.Service.ExitCodeKind, st.Service.ExitCode)
		v.Problem = "The game server keeps crashing on startup — systemd has restarted it " +
			fmt.Sprint(st.Service.RestartCount) + " times in the last minute and then stopped trying."
		v.ProblemFix = "Read the server log below for the crash reason. A missing steamclient.so under ~/.steam/sdk64 is the usual cause on a fresh install."
	}
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
		if v.Hostname == "" {
			v.Hostname = st.Rcon.Hostname
		}
		v.PlayerList = make([]web.PlayerRow, 0, len(st.Rcon.Players))
		for _, p := range st.Rcon.Players {
			v.PlayerList = append(v.PlayerList, web.PlayerRow{
				Name:      p.Name,
				Addr:      p.Addr,
				SteamID:   p.SteamID,
				Connected: p.Connected,
				Ping:      p.Ping,
				State:     p.State,
				// CS2's status table carries no SteamID for anyone, so "no
				// SteamID" cannot mean "bot" any more — the engine reports that
				// separately, and inferring it here labelled every human a BOT.
				IsBot: p.Bot,
			})
		}
		// When A2S did not answer, RCON is the only source of the counts. The
		// header used to read "0 players" while the list below it showed the
		// people who were connected.
		if st.Info == nil {
			v.Players = st.Rcon.Humans + st.Rcon.Bots
			v.Bots = st.Rcon.Bots
			// CS2 always prints "(0 max)" in status, so a zero here is "unknown",
			// not "no slots".
			if st.Rcon.Max > 0 {
				v.Max = st.Rcon.Max
			}
		}
	}
	if st.Service.UptimeSeconds > 0 {
		v.UptimeLabel = uptimeLabel(st.Service.UptimeSeconds)
	}
	// A diagnosed RCON problem is shown as a problem with a fix, not as a raw
	// note: "dial 127.0.0.1:27015: connection refused" told the operator
	// nothing they could act on.
	if st.Diag != nil && !st.Diag.OK {
		v.Problem = capitalize(st.Diag.Reason) + "."
		if st.Diag.Fix != "" {
			v.ProblemFix = capitalize(st.Diag.Fix) + "."
		}
		v.CanRepair = st.Diag.Repairable
	} else if st.Note != "" {
		v.Note = st.Note
	}
	if polled {
		return v
	}
	if maps, err := s.agent.Maps(ctx); err == nil {
		v.Maps = maps
		v.CurrentMap = v.Map
	}
	// The journal is the first thing anyone needs when a server will not start,
	// and it is only fetched for admins (it can contain connection details).
	if v.IsAdmin {
		if lines, err := s.agent.Logs(ctx, 40); err == nil {
			v.LogLines = lines
		}
	}
	return v
}

// capitalize upper-cases the first letter so agent messages read as sentences.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func boolLabel(b bool) string {
	if b {
		return "running"
	}
	return "stopped"
}

// exitStatusLabel describes how the last run of the game binary ended, in
// operator terms: systemd reports "killed" + 11 for a segfault and "exited" +
// 1 for a clean non-zero exit.
func exitStatusLabel(kind string, code int) string {
	return web.ExitCodeLabel(kind, code)
}

// --- pages ---------------------------------------------------------------

func (s *Server) handleServerPage(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	v := s.buildServerView(r, u)
	comp := web.Base("Server", s.navFor(r, u, "server"), web.ServerPage(s.navFor(r, u, "server"), flash(r), v))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render server page", "err", err)
	}
}

func (s *Server) handleStatusCardPartial(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	// The polled view also carries the lifecycle row and the player list
	// out-of-band, so they cannot drift out of sync with the status the operator
	// is looking at.
	v := s.serverView(r, u, true)
	if err := web.StatusCard(v).Render(r.Context(), w); err != nil {
		s.log.Error("render status partial", "err", err)
	}
}

// handleServerLogsPartial re-renders just the log card. It does not build the
// whole server view: the log panel needs nothing but the journal and whether
// the server is up (polling a stopped server's frozen journal is pure noise —
// the poll loop stops by the swapped-in card not asking for the next tick).
// One cheap agent call answers both.
func (s *Server) handleServerLogsPartial(w http.ResponseWriter, r *http.Request) {
	v := web.ServerView{IsAdmin: true}
	if st, err := s.agent.Status(r.Context()); err == nil {
		v.Online = st.Service.Active && !st.Service.CrashLooping
	}
	if lines, err := s.agent.Logs(r.Context(), 40); err == nil {
		v.LogLines = lines
	}
	if err := web.ServerLogs(v).Render(r.Context(), w); err != nil {
		s.log.Error("render logs partial", "err", err)
	}
}

// --- server actions -------------------------------------------------------

// handleServerAction performs a lifecycle action. The agent verifies the unit
// actually reached the requested state, so the flash reports what happened
// instead of the old optimistic "Server starting…" that appeared even when the
// unit died immediately.
func (s *Server) handleServerAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFromCtx(r)
		res, err := s.agent.ServerAction(r.Context(), action)
		if err != nil {
			msg := actionError(action, err)
			if res != nil && len(res.Log) > 0 {
				msg += " Last log lines: " + lastLogLine(res.Log)
			}
			redirectFlash(w, r, "/", "err", msg)
			return
		}
		s.store.Audit(u.Username, "server."+action, "")
		msg := actionMessage(action)
		if res != nil && res.Message != "" {
			msg = res.Message
		}
		redirectFlash(w, r, "/", "ok", msg)
	}
}

// lastLogLine picks the most useful journal line for a one-line flash: the last
// non-empty entry, with systemd's timestamp/host/unit prefix removed.
func lastLogLine(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		// "Sep 05 12:00:00 host cs2-server[123]: message"
		if idx := strings.Index(l, "]: "); idx > 0 {
			l = l[idx+3:]
		}
		return l
	}
	return ""
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
	return "Failed to " + action + " the server: " + err.Error()
}

// handleRCONRepair applies the RCON fixes the agent diagnosed.
func (s *Server) handleRCONRepair(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	applied, res, diag, err := s.agent.RCONRepair(r.Context())
	if err != nil {
		redirectFlash(w, r, "/", "err", "Could not repair the server connection: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "server.rcon.repair", strings.Join(applied, "; "))
	switch {
	case len(applied) == 0:
		redirectFlash(w, r, "/", "ok", firstNonEmpty(res.Message, "Nothing needed repairing."))
	case diag != nil && diag.OK:
		redirectFlash(w, r, "/", "ok", "Fixed: "+joinHuman(applied)+". The server connection works now.")
	default:
		msg := "Applied: " + joinHuman(applied) + "."
		if diag != nil && diag.Reason != "" {
			msg += " Still not reachable: " + diag.Reason + "."
		} else {
			msg += " The server is restarting; give it a minute to finish loading."
		}
		redirectFlash(w, r, "/", "ok", msg)
	}
}

// joinHuman renders a list as "a, b and c".
func joinHuman(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
		// The client's message already says the agent cannot be reached and
		// how to check it — an "Agent unreachable: cannot reach the cs2a
		// agent — …" line said the same thing twice.
		redirectFlash(w, r, "/", "err", err.Error())
		return nil
	}
	return pluginCardViews(entries)
}

// pluginCardViews maps agent catalog entries to card models.
func pluginCardViews(entries []PluginEntry) []web.PluginCardView {
	out := make([]web.PluginCardView, 0, len(entries))
	for _, e := range entries {
		out = append(out, web.PluginCardView{
			ID:          e.ID,
			Name:        e.Name,
			Author:      e.Author,
			Kind:        e.Kind,
			Homepage:    e.Homepage,
			HasConfig:   e.ConfigPath != "",
			Requires:    e.Requires,
			Description: e.Description,
			Installed:   e.Installed,
			Version:     e.InstalledVersion,
			RequiredBy:  e.RequiredBy,
		})
	}
	return out
}

func (s *Server) handlePluginsPage(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	entries := s.pluginCatalogCards(w, r)
	if entries == nil {
		return // redirected with flash
	}
	jobs := s.pluginJobViews(r)
	comp := web.Base("Plugins", s.navFor(r, u, "plugins"), web.PluginsPage(s.navFor(r, u, "plugins"), flash(r), entries, jobs))
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
			Message: humanJobError(j.Message),
			Running: j.Running(),
		}
		v.DownloadProgress(j.DownloadBytes, j.DownloadTotal)
		if j.Result != nil {
			v.Version = j.Result.Version
			// "Restart the server to load it" is only true until the first
			// restart after the install; the agent says when that happened.
			v.RequiresRestart = j.Result.RequiresRestart && !j.RestartObserved
			v.Warning = j.Result.Warning
		}
		out = append(out, v)
	}
	return out
}

// humanJobError strips the internal "plugins: …" prefixes the agent's error
// chain accumulates. The user saw
// "plugins: dependency cssharp: plugins: dependency metamod: plugins: metamod:
// read pointer: Get …" — every prefix in that string was noise. They appear
// mid-sentence too (a dependency failure wraps the inner install error, whose
// own message starts with the package prefix), so every occurrence goes, not
// just the leading ones.
func humanJobError(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	msg = strings.ReplaceAll(msg, "plugins: ", "")
	msg = strings.Join(strings.Fields(msg), " ") // collapse the double gaps left behind
	return capitalize(msg)
}

// handlePluginJobsPartial is polled by the plugins page while an install runs.
//
// When nothing is running any more it also swaps a fresh copy of the card grid
// out-of-band: an install that just finished has to flip its card to
// "installed" (and enable Configure), which previously needed a manual reload.
func (s *Server) handlePluginJobsPartial(w http.ResponseWriter, r *http.Request) {
	jobs := s.pluginJobViews(r)
	if err := web.PluginJobs(jobs).Render(r.Context(), w); err != nil {
		s.log.Error("render plugin jobs", "err", err)
		return
	}
	if web.AnyRunning(jobs) {
		return // still working; the cards cannot have changed yet
	}
	entries, err := s.agent.Plugins(r.Context())
	if err != nil {
		return // the strip already rendered; a stale grid is not worth an error
	}
	if err := web.PluginCards(pluginCardViews(entries), true).Render(r.Context(), w); err != nil {
		s.log.Error("render plugin cards oob", "err", err)
	}
}

func (s *Server) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	id := r.PathValue("id")
	// Installs run as a background job on the agent: downloads take minutes,
	// which no HTTP request (or reverse proxy) should be asked to hold open.
	job, err := s.agent.InstallAsync(r.Context(), id, false)
	if err != nil {
		redirectFlash(w, r, "/plugins", "err", "Install failed: "+humanJobError(err.Error()))
		return
	}
	s.store.Audit(u.Username, "plugin.install.start", id+" job="+job.ID)
	// The install job changes what the nav can offer (WeaponPaints gates the
	// Loadout tab); dropping the caps cache now means the tab follows the
	// finished job instead of a stale answer.
	s.caps.reset()
	redirectFlash(w, r, "/plugins", "ok", "Installing "+id+" — progress appears below, you can leave this page.")
}

func (s *Server) handlePluginUninstall(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	id := r.PathValue("id")
	if err := s.agent.Uninstall(r.Context(), id); err != nil {
		// A conflict is a "not now", not a failure: the operator has to wait for
		// a running install rather than fix anything. Labelling it "Uninstall
		// failed" sent people looking for a broken plugin.
		var api *APIError
		if errors.As(err, &api) && api.Status == http.StatusConflict {
			redirectFlash(w, r, "/plugins", "err", humanJobError(api.Message))
			return
		}
		// The agent's refusal ("… is required by CounterStrikeSharp") is the
		// useful part; its internal "plugins: " prefixes are not.
		redirectFlash(w, r, "/plugins", "err", "Uninstall failed: "+humanJobError(err.Error()))
		return
	}
	s.store.Audit(u.Username, "plugin.uninstall", id)
	s.caps.reset()
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
	comp := web.Base("Config", s.navFor(r, u, "plugins"), web.PluginConfigPage(s.navFor(r, u, "plugins"), flash(r), v))
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
		comp := web.Base("Config", s.navFor(r, u, "plugins"), web.PluginConfigPage(s.navFor(r, u, "plugins"), &flash, v))
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

	if settings, warning, err := s.agent.Settings(r.Context()); err == nil {
		for _, set := range settings {
			if set.Name == "sv_password" && set.Value != "0" && set.Value != "" {
				v.Password = set.Value
			}
		}
		v.CFGWarning = warning
	}
	// Enforcement lives in the whitelist plugin's own config, not in a cvar.
	// The card only exists once the plugin does: an "inactive — requires the
	// CS2 Whitelist plugin" card from day one is noise on a fresh install.
	var users []User
	if list, err := s.store.ListUsers(); err == nil {
		users = list
	}
	if st, err := s.agent.WhitelistState(r.Context()); err == nil {
		v.WhitelistInstalled = st.Installed
		if st.Installed {
			v.WhitelistText = strings.Join(st.SteamIDs, "\n")
			v.WhitelistActive = st.Enabled
			v.WhitelistCount = len(st.SteamIDs)
			v.WhitelistPlayers = whitelistPlayerRows(st.SteamIDs, users)
			v.AddableUsers = addableWhitelistUsers(users, st.SteamIDs)
		}
	}
	comp := web.Base("Access", s.navFor(r, u, "access"), web.AccessPage(s.navFor(r, u, "access"), flash(r), v))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render access", "err", err)
	}
}

// whitelistPlayerRows turns the raw SteamID list into rows the card can
// show, resolving the linked panel account's username where one matches. A
// bare textarea of ids made admins decode 17-digit numbers to answer "is my
// friend on the list?".
func whitelistPlayerRows(ids []string, users []User) []web.WhitelistPlayer {
	bySteam := map[string]string{}
	for _, uu := range users {
		if uu.SteamID64 != "" {
			bySteam[uu.SteamID64] = uu.Username
		}
	}
	out := make([]web.WhitelistPlayer, 0, len(ids))
	for _, id := range ids {
		out = append(out, web.WhitelistPlayer{SteamID: id, Name: bySteam[id]})
	}
	return out
}

// addableWhitelistUsers picks the panel users the add-players modal offers:
// a linked SteamID is required (nothing to whitelist otherwise) and the id
// must not already be on the list (adding a listed player again is noise).
func addableWhitelistUsers(users []User, ids []string) []web.UserRow {
	onList := map[string]bool{}
	for _, id := range ids {
		onList[id] = true
	}
	var out []web.UserRow
	for _, uu := range users {
		if uu.SteamID64 == "" || onList[uu.SteamID64] {
			continue
		}
		out = append(out, web.UserRow{
			ID: uu.ID, Username: uu.Username, Role: uu.Role, SteamID: uu.SteamID64,
			Created: uu.CreatedAt.Format("2006-01-02"),
		})
	}
	return out
}

// handleAccessWhitelistToggle switches whitelist enforcement on or off.
func (s *Server) handleAccessWhitelistToggle(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	on := r.FormValue("enabled") == "1"
	// Enforcing an empty whitelist locks every player out, including the
	// operator flipping the switch. The page warns about it in prose; refusing
	// the click is what actually prevents it.
	if on {
		if st, err := s.agent.WhitelistState(r.Context()); err == nil && len(st.SteamIDs) == 0 {
			redirectFlash(w, r, "/access", "err",
				"Add at least one SteamID before enforcing the whitelist — an empty enforced list rejects everyone, including you.")
			return
		}
	}
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
	lockedNow, err := s.agent.SetPassword(r.Context(), pw)
	if err != nil {
		redirectFlash(w, r, "/access", "err", "Could not set password: "+err.Error())
		return
	}
	var detail string
	switch {
	case pw == "" && lockedNow:
		detail = "cleared — the map reloaded, the server is public again"
	case pw == "":
		detail = "cleared — it applies when the server next starts (it is offline right now)"
	case lockedNow:
		detail = "set — the map reloaded so it locks right away"
	default:
		detail = "set — it applies when the server next starts (it is offline right now)"
	}
	s.store.Audit(u.Username, "access.password", detail)
	redirectFlash(w, r, "/access", "ok", "Server password "+detail+".")
}

// handleAccessWhitelist appends hand-entered SteamIDs to the list. The card's
// textarea once replaced the whole list — one typo in a re-typed list removed
// players silently. Appending can only add.
func (s *Server) handleAccessWhitelist(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	raw := r.FormValue("steamids")
	var ids []string
	for _, line := range strings.Split(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			ids = append(ids, line)
		}
	}
	if len(ids) == 0 {
		redirectFlash(w, r, "/access", "err", "Nothing to add — enter at least one SteamID.")
		return
	}
	s.appendWhitelist(w, r, u, ids, "access.whitelist.add", fmt.Sprintf("%d entries", len(ids)))
}

// appendWhitelist fetches the current list, appends the given ids, dedupes,
// saves, audits and redirects. Shared by the hand-entry form and the
// add-players modal; auditDetail describes the add for the audit log.
func (s *Server) appendWhitelist(w http.ResponseWriter, r *http.Request, u *User, ids []string, auditAction, auditDetail string) {
	current, err := s.agent.Whitelist(r.Context())
	if err != nil {
		redirectFlash(w, r, "/access", "err", err.Error())
		return
	}
	seen := map[string]bool{}
	for _, id := range current {
		seen[id] = true
	}
	added := 0
	merged := append([]string(nil), current...)
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			merged = append(merged, id)
			added++
		}
	}
	if added == 0 {
		redirectFlash(w, r, "/access", "ok", "Already on the whitelist — nothing to add.")
		return
	}
	if err := s.agent.PutWhitelist(r.Context(), merged); err != nil {
		redirectFlash(w, r, "/access", "err", "Whitelist save failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, auditAction, auditDetail)
	redirectFlash(w, r, "/access", "ok", fmt.Sprintf("Added %d %s (normalized to SteamID64).", added, web.Plural(added, "player", "players")))
}

// handleAccessWhitelistRemove takes one SteamID off the list. The agent's
// Apply keeps the last non-empty guarantee (it refuses emptying an enforced
// list), so removing the final entry with enforcement on surfaces as an
// error rather than silently locking everyone out.
func (s *Server) handleAccessWhitelistRemove(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	id := strings.TrimSpace(r.FormValue("steamid"))
	if id == "" {
		redirectFlash(w, r, "/access", "err", "No SteamID to remove.")
		return
	}
	current, err := s.agent.Whitelist(r.Context())
	if err != nil {
		redirectFlash(w, r, "/access", "err", err.Error())
		return
	}
	kept := make([]string, 0, len(current))
	removed := false
	for _, cur := range current {
		if cur == id {
			removed = true
			continue
		}
		kept = append(kept, cur)
	}
	if !removed {
		redirectFlash(w, r, "/access", "ok", "That SteamID was not on the whitelist.")
		return
	}
	if err := s.agent.PutWhitelist(r.Context(), kept); err != nil {
		redirectFlash(w, r, "/access", "err", "Whitelist save failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "access.whitelist.remove", id)
	redirectFlash(w, r, "/access", "ok", "Removed from the whitelist.")
}

// handleAccessWhitelistAddUsers is the add-players modal's target: one or
// many panel users, checked off in the dialog, appended by their linked
// SteamIDs. The single-user handler this replaces did the same thing one
// POST at a time.
func (s *Server) handleAccessWhitelistAddUsers(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	if err := r.ParseForm(); err != nil {
		redirectFlash(w, r, "/access", "err", "Bad form.")
		return
	}
	idStrs := r.Form["user_id"]
	if len(idStrs) == 0 {
		redirectFlash(w, r, "/access", "err", "Pick at least one player.")
		return
	}
	var ids []string
	var names []string
	for _, idStr := range idStrs {
		var userID int64
		if _, err := fmt.Sscanf(idStr, "%d", &userID); err != nil {
			continue
		}
		target, err := s.store.GetUserByID(userID)
		if err != nil || target.SteamID64 == "" {
			continue
		}
		ids = append(ids, target.SteamID64)
		names = append(names, target.Username)
	}
	if len(ids) == 0 {
		redirectFlash(w, r, "/access", "err", "None of those users has a linked SteamID.")
		return
	}
	s.appendWhitelist(w, r, u, ids, "access.whitelist.add", strings.Join(names, ", "))
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
	comp := web.Base("Users", s.navFor(r, u, "users"), web.UsersPage(s.navFor(r, u, "users"), flash(r), v, u.ID))
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

// handleUserRole toggles a user between admin and player. The actor cannot
// change their own role (an admin demoting themselves is how panels end up
// adminless, and "promote yourself" needs no button), and the last admin
// cannot be demoted (the store refuses; the handler says it in human words).
func (s *Server) handleUserRole(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	var userID int64
	if _, err := fmt.Sscanf(r.FormValue("user_id"), "%d", &userID); err != nil {
		redirectFlash(w, r, "/users", "err", "Invalid user id.")
		return
	}
	if userID == u.ID {
		redirectFlash(w, r, "/users", "err", "You cannot change your own role.")
		return
	}
	target, err := s.store.GetUserByID(userID)
	if err != nil {
		redirectFlash(w, r, "/users", "err", "User not found.")
		return
	}
	newRole := "player"
	if target.Role == "player" {
		newRole = "admin"
	}
	if err := s.store.SetUserRole(userID, newRole); err != nil {
		redirectFlash(w, r, "/users", "err", "Could not change role: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "user.role", target.Username+" -> "+newRole)
	redirectFlash(w, r, "/users", "ok", target.Username+" is now "+newRole+".")
}

// --- settings -----------------------------------------------------------------

// handleSettingsPage renders the curated cvar catalog with the current managed
// values filled in. sv_password is deliberately absent (Access page owns it);
// its row in the managed block is preserved untouched by every save here.
func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r)
	v := web.SettingsView{Groups: web.SettingsCatalog()}

	current := map[string]string{}
	if settings, warning, err := s.agent.Settings(r.Context()); err == nil {
		v.CFGWarning = warning
		for _, set := range settings {
			current[set.Name] = set.Value
		}
		// Non-catalog rows are preserved by the save; the page says how many
		// exist so a save does not look like it might drop them.
		for _, set := range settings {
			if web.SpecByName(set.Name) == nil && set.Name != "sv_password" {
				v.ExtraCount++
			}
		}
	} else {
		v.AgentDown = true
	}
	for gi := range v.Groups {
		for ri := range v.Groups[gi].Rows {
			name := v.Groups[gi].Rows[ri].Name
			if val, ok := current[name]; ok {
				v.Groups[gi].Rows[ri].Value = val
				v.Groups[gi].Rows[ri].HasValue = true
			}
		}
	}
	comp := web.Base("Settings", s.navFor(r, u, "settings"), web.SettingsPage(s.navFor(r, u, "settings"), flash(r), v))
	if err := comp.Render(r.Context(), w); err != nil {
		s.log.Error("render settings", "err", err)
	}
}

// handleSettingsPost validates every catalog field, merges with the managed
// block (keeping sv_password and any non-catalog rows), and pushes the result
// to the agent, which writes server.cfg and applies it live over one RCON
// connection.
//
// The form posts every field, each already showing the value it will save
// (the operator's when set, the standard competitive default otherwise), so
// saving one changed field never trips over the empty ones — that
// one-error-per-field-at-a-time dance is exactly the bug this replaces.
// Missing fields (a crafted POST without them) fall back the same way, so
// the page and the file can never disagree about what a save means.
func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	s.saveSettings(w, r, nil)
}

// handleSettingsReset writes the full standard competitive set (the catalog's
// Default for every row), replacing every curated value the operator had
// saved. sv_password and non-catalog rows in the managed block survive, as
// in a normal save.
func (s *Server) handleSettingsReset(w http.ResponseWriter, r *http.Request) {
	defaults := map[string]string{}
	for _, spec := range web.AllSettingSpecs() {
		defaults[spec.Name] = spec.Default
	}
	s.saveSettings(w, r, defaults)
}

// saveSettings is the shared write path for the settings page. overrides, when
// non-nil, replaces every catalog value (the reset button); otherwise values
// come from the posted form, defaulting to the catalog's standard for fields
// that are missing or blank.
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request, overrides map[string]string) {
	if err := r.ParseForm(); err != nil {
		redirectFlash(w, r, "/settings", "err", "Bad form.")
		return
	}
	// Start from what is in the file: anything the panel does not own must
	// survive this save byte for byte.
	current := []Setting{}
	if settings, _, err := s.agent.Settings(r.Context()); err == nil {
		current = settings
	} else {
		redirectFlash(w, r, "/settings", "err", "Could not read the current settings: "+err.Error())
		return
	}

	// Resolve a value for every catalog row, in order of authority:
	//
	//   1. an override (the reset button pins every field to its standard),
	//   2. the posted field when it carries a value,
	//   3. the current file value — a field left blank or missing from the
	//      POST must not lose what the operator already saved,
	//   4. the standard competitive default when the cvar has never been
	//      set at all.
	//
	// Rules 3 and 4 are what fix the fresh-install save: before them, a blank
	// field was "rejected: must be a whole number" — one error per save
	// until the whole form was filled. Now the field already shows the value
	// rule 3 or 4 will apply, and a blank simply means "keep that".
	specs := web.AllSettingSpecs()
	currentByName := make(map[string]string, len(current))
	for _, set := range current {
		currentByName[set.Name] = set.Value
	}
	posted := map[string]string{}
	for _, spec := range specs {
		var val string
		if v, ok := overrides[spec.Name]; ok {
			val = v
		} else if v := strings.TrimSpace(r.PostFormValue("set_" + spec.Name)); v != "" {
			val = v
		} else if v := currentByName[spec.Name]; v != "" {
			val = v
		} else {
			val = spec.Default
		}
		if val == "" {
			continue // nothing standard to apply; the current row survives as-is
		}
		posted[spec.Name] = val
	}

	// Validate every value before writing anything: one bad field must not
	// half-apply a batch.
	updates := make([]Setting, 0, len(specs))
	for _, spec := range specs {
		val, ok := posted[spec.Name]
		if !ok {
			continue
		}
		if err := web.ValidateSettingValue(&spec, val); err != nil {
			redirectFlash(w, r, "/settings", "err", spec.Label+": "+err.Error())
			return
		}
		updates = append(updates, Setting{Name: spec.Name, Value: val, Comment: "managed by cs2a"})
	}

	// Merge: current block order first (sv_password + non-catalog rows keep
	// their place and comments), then catalog rows that were not present.
	merged := make([]Setting, 0, len(current)+len(updates))
	seen := make(map[string]bool, len(current)+len(updates))
	for _, set := range current {
		if spec := web.SpecByName(set.Name); spec != nil {
			// A curated row: the resolved value wins.
			if val, ok := posted[set.Name]; ok {
				merged = append(merged, Setting{Name: set.Name, Value: val, Comment: set.Comment})
				seen[set.Name] = true
				continue
			}
			// Not resolved (nothing posted and no default — impossible with
			// today's catalog, but be safe): keep the current value.
			merged = append(merged, set)
			seen[set.Name] = true
			continue
		}
		// sv_password and unknown rows survive as-is.
		merged = append(merged, set)
		seen[set.Name] = true
	}
	for _, set := range updates {
		if !seen[set.Name] {
			merged = append(merged, set)
		}
	}

	warning, err := s.agent.PutSettings(r.Context(), merged)
	if err != nil {
		redirectFlash(w, r, "/settings", "err", "Save failed: "+err.Error())
		return
	}
	u := userFromCtx(r)
	if overrides != nil {
		s.store.Audit(u.Username, "settings.reset", "competitive defaults")
	} else {
		s.store.Audit(u.Username, "settings.save", fmt.Sprintf("%d settings", len(updates)))
	}
	msg := "Settings saved — written to server.cfg and pushed live."
	if overrides != nil {
		msg = "Settings reset to the standard competitive rules — written to server.cfg and pushed live."
	}
	if warning != "" {
		msg = "Saved, but: " + warning
	}
	redirectFlash(w, r, "/settings", "ok", msg)
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
	// catalogs (gloves/agents/weapons) come from the agent; fall back to empty lists
	if gloves, agentsT, agentsCT, weapons, err := s.agent.Cosmetics(r.Context()); err == nil {
		for _, g := range gloves {
			v.Gloves = append(v.Gloves, web.GloveOption{Value: gloveValue(g.Defindex, g.Paint), Label: g.Name, Image: g.Image})
		}
		for _, a := range agentsT {
			v.AgentsT = append(v.AgentsT, web.AgentOption{Value: a.Model, Label: a.Name, Image: a.Image})
		}
		for _, a := range agentsCT {
			v.AgentsCT = append(v.AgentsCT, web.AgentOption{Value: a.Model, Label: a.Name, Image: a.Image})
		}
		for _, w := range weapons {
			def := strconv.Itoa(w.Defindex)
			wo := web.WeaponOption{
				Defindex:      w.Defindex,
				Name:          w.Name,
				Team:          w.Team,
				NameT:         "skin_t[" + def + "]",
				NameCT:        "skin_ct[" + def + "]",
				DefindexLabel: def,
			}
			for _, sk := range w.Skins {
				wo.Skins = append(wo.Skins, web.WeaponSkinOption{Value: strconv.Itoa(sk.Paint), Label: sk.Name, Image: sk.Image})
			}
			v.Weapons = append(v.Weapons, wo)
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
			v.SkinT = lo.SkinsT
			v.SkinCT = lo.SkinsCT
			v.SyncEnabled = lo.SyncEnabled
		}
	}
	comp := web.Base("Loadout", s.navFor(r, u, "loadout"), web.LoadoutPage(s.navFor(r, u, "loadout"), flash(r), v))
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
		SkinsT:   s.validSkins(r, "skin_t"),
		SkinsCT:  s.validSkins(r, "skin_ct"),
	}
	syncEnabled, warning, err := s.agent.PutLoadout(r.Context(), u.SteamID64, lo)
	if err != nil {
		redirectFlash(w, r, "/loadout", "err", "Save failed: "+err.Error())
		return
	}
	s.store.Audit(u.Username, "loadout.save", "t="+lo.KnifeT+" ct="+lo.KnifeCT+" gloves="+lo.GlovesT+"/"+lo.GlovesCT+" agents="+lo.AgentT+"/"+lo.AgentCT)
	if warning != "" {
		// The selection is stored; only the push into WeaponPaints' database
		// failed. Reporting it as a failed save sent players into a retry loop
		// over something only an admin can fix.
		redirectFlash(w, r, "/loadout", "err", "Loadout "+warning)
		return
	}
	if !syncEnabled {
		// Promising "it applies when you reconnect" is wrong when WeaponPaints
		// has no database: nothing reaches the game server at all.
		redirectFlash(w, r, "/loadout", "ok",
			"Loadout saved to the panel — but skins sync is not set up yet, so it will not appear in game until an admin configures WeaponPaints.")
		return
	}
	redirectFlash(w, r, "/loadout", "ok", "Loadout saved — it applies when you (re)connect. Use !wp in game to force a refresh.")
}

// validSkins collects the per-weapon skin picks ("skin_t[7]=421") from the
// form. Values are validated against the agent's weapon catalog — a made-up
// paint id would write a row WeaponPaints cannot render, and a made-up
// defindex a row it would never read. A missing catalog (agent unreachable)
// fails the save elsewhere; here it means no picks survive, which is the safe
// direction: no rows are written from unvalidated input.
func (s *Server) validSkins(r *http.Request, prefix string) map[string]string {
	out := map[string]string{}
	if err := r.ParseForm(); err != nil {
		return out
	}
	var catalog map[string]map[string]bool // defindex -> paint ids
	buildCatalog := func() {
		if catalog != nil {
			return
		}
		catalog = map[string]map[string]bool{}
		if _, _, _, weapons, err := s.agent.Cosmetics(r.Context()); err == nil {
			for _, w := range weapons {
				paints := map[string]bool{}
				for _, sk := range w.Skins {
					paints[strconv.Itoa(sk.Paint)] = true
				}
				catalog[strconv.Itoa(w.Defindex)] = paints
			}
		}
	}
	for key, vals := range r.Form {
		if !strings.HasPrefix(key, prefix+"[") || !strings.HasSuffix(key, "]") {
			continue
		}
		def := strings.TrimSuffix(strings.TrimPrefix(key, prefix+"["), "]")
		if def == "" || len(vals) == 0 {
			continue
		}
		paint := strings.TrimSpace(vals[len(vals)-1])
		if paint == "" {
			// Explicit "no skin": record it so the agent deletes the row.
			out[def] = ""
			continue
		}
		buildCatalog()
		if paints, ok := catalog[def]; ok && paints[paint] {
			out[def] = paint
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
