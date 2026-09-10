package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cs2a/internal/rcon"
)

// API is the agent's HTTP surface. It binds to loopback only; the panel is
// the sole intended client, authenticating with the shared bearer token.
type API struct {
	cfg     Config
	server  *Server
	wh      *Whitelist
	inst    *Installer
	loadout *LoadoutStore
	jobs    *Jobs
	// updater serves the CS2 build check/update endpoints. Optional so tests
	// that do not exercise updates can construct an API without one.
	updater *Updater
}

// NewAPI wires the HTTP API.
func NewAPI(cfg Config, srv *Server, wh *Whitelist, inst *Installer, loadout *LoadoutStore) *API {
	return &API{cfg: cfg, server: srv, wh: wh, inst: inst, loadout: loadout, jobs: NewJobs()}
}

// WithUpdater attaches the CS2 updater; nil disables the update endpoints
// (they answer "no updater configured" rather than panicking).
func (a *API) WithUpdater(u *Updater) *API {
	a.updater = u
	return a
}

// InstallPending enqueues bootstrap's "recommended stack" as install jobs,
// started sequentially from one background driver so a dependency is fully
// on disk before its dependent begins (the installer would resolve deps
// itself, but then the Plugins page shows one giant job instead of the
// operator's own choice walking down the list).
//
// It returns the ids it recognized (unknown ids are dropped: a bootstrap this
// build predates could not install them anyway) and returns immediately; the
// driver runs on and calls done exactly once with the ids that did NOT
// install, so the caller can keep exactly those pending in the config. A
// Valve update day that breaks an upstream release is retried by the next
// boot instead of being silently forgotten.
func (a *API) InstallPending(ids []string, done func(failed []string)) []string {
	// Already-installed ids are skipped: a bootstrap rerun re-offers the
	// stack, and reinstalling what is on disk would churn the game tree for
	// nothing (and race whatever the operator is doing from the panel).
	var known []string
	for _, id := range ids {
		if _, ok := Find(a.inst.catalog, id); !ok {
			continue
		}
		if a.inst.IsInstalled(id) {
			continue
		}
		known = append(known, id)
	}
	if len(known) == 0 {
		if done != nil {
			done(nil)
		}
		return nil
	}
	go func() {
		var failed []string
		defer func() {
			if done != nil {
				done(failed)
			}
		}()
		for _, id := range known {
			entry, _ := Find(a.inst.catalog, id)
			job, err := a.jobs.Start("install", id, entry.Name, func(ctx context.Context, prog func(Progress)) (*InstallResult, error) {
				res, err := a.inst.InstallProgress(ctx, id, false, prog)
				if err != nil {
					return nil, err
				}
				return &res, nil
			})
			if err != nil {
				// Target busy: a panel install won the race and is doing the
				// work; count it as handled either way.
				continue
			}
			// Drain this job before starting the next: installs share the
			// game tree, and a failed root dependency should fail its
			// dependents in their own jobs rather than overlap with them.
			for {
				j, ok := a.jobs.Get(job.ID)
				if !ok || j.Status != JobRunning {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			if j, ok := a.jobs.Get(job.ID); ok && j.Status != JobDone {
				failed = append(failed, id)
			}
		}
	}()
	return known
}

// Handler builds the agent's http.Handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// unauthenticated health probe (no sensitive data)
	mux.HandleFunc("GET /api/v1/health", a.handleHealth)

	// everything below requires the bearer token
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return a.auth(h)
	}
	mux.HandleFunc("GET /api/v1/status", auth(a.handleStatus))
	mux.HandleFunc("GET /api/v1/server/update", auth(a.handleGetUpdate))
	mux.HandleFunc("POST /api/v1/server/update", auth(a.handleServerUpdate))
	mux.HandleFunc("POST /api/v1/server/update/check", auth(a.handleUpdateCheck))
	mux.HandleFunc("POST /api/v1/server/start", auth(a.action(ActionStart)))
	mux.HandleFunc("POST /api/v1/server/stop", auth(a.action(ActionStop)))
	mux.HandleFunc("POST /api/v1/server/restart", auth(a.action(ActionRestart)))
	mux.HandleFunc("POST /api/v1/server/exec", auth(a.handleExec))
	mux.HandleFunc("GET /api/v1/server/logs", auth(a.handleLogs))
	mux.HandleFunc("GET /api/v1/server/rcon-check", auth(a.handleRCONCheck))
	mux.HandleFunc("POST /api/v1/server/rcon-repair", auth(a.handleRCONRepair))
	mux.HandleFunc("GET /api/v1/maps", auth(a.handleMaps))
	mux.HandleFunc("GET /api/v1/cosmetics", auth(a.handleCosmetics))
	mux.HandleFunc("POST /api/v1/map", auth(a.handleMapChange))
	mux.HandleFunc("GET /api/v1/settings", auth(a.handleGetSettings))
	mux.HandleFunc("PUT /api/v1/settings", auth(a.handlePutSettings))
	mux.HandleFunc("PUT /api/v1/password", auth(a.handlePassword))
	mux.HandleFunc("GET /api/v1/plugins", auth(a.handlePlugins))
	mux.HandleFunc("POST /api/v1/plugins/{id}/install", auth(a.handlePluginInstall))
	mux.HandleFunc("DELETE /api/v1/plugins/{id}", auth(a.handlePluginUninstall))
	mux.HandleFunc("GET /api/v1/jobs", auth(a.handleListJobs))
	mux.HandleFunc("GET /api/v1/jobs/{id}", auth(a.handleGetJob))
	mux.HandleFunc("GET /api/v1/whitelist", auth(a.handleGetWhitelist))
	mux.HandleFunc("PUT /api/v1/whitelist", auth(a.handlePutWhitelist))
	mux.HandleFunc("PUT /api/v1/whitelist/enabled", auth(a.handlePutWhitelistEnabled))
	mux.HandleFunc("GET /api/v1/loadout/{steamid}", auth(a.handleGetLoadout))
	mux.HandleFunc("PUT /api/v1/loadout/{steamid}", auth(a.handlePutLoadout))
	mux.HandleFunc("GET /api/v1/plugins/{id}/config", auth(a.handleGetPluginConfig))
	mux.HandleFunc("PUT /api/v1/plugins/{id}/config", auth(a.handlePutPluginConfig))

	return mux
}

func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(a.cfg.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

// --- handlers ---------------------------------------------------------

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "cs2a-agent"})
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.server.Status(r.Context()))
}

// handleGetUpdate reports the cached build comparison without touching
// Steam. The status poller can call this every five seconds; a check that
// spawned steamcmd each time would be a self-inflicted rate limit.
func (a *API) handleGetUpdate(w http.ResponseWriter, r *http.Request) {
	if a.updater == nil {
		writeErr(w, http.StatusNotFound, "no updater configured")
		return
	}
	writeJSON(w, http.StatusOK, a.updater.Info())
}

// handleUpdateCheck forces a fresh comparison now (steamcmd app_info) and
// returns it. This is the panel's "Check for update" button; it is
// synchronous because a check is seconds, unlike an update's minutes.
func (a *API) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if a.updater == nil {
		writeErr(w, http.StatusNotFound, "no updater configured")
		return
	}
	info := a.updater.Check(r.Context(), true)
	code := http.StatusOK
	if info.LastError != "" {
		code = http.StatusConflict // the answer exists, but half of it failed
	}
	writeJSON(w, code, info)
}

// handleServerUpdate applies the update as a background job: stopping the
// game server, downloading ~a few GB and restarting can take tens of
// minutes, which no request should hold open.
func (a *API) handleServerUpdate(w http.ResponseWriter, r *http.Request) {
	if a.updater == nil {
		writeErr(w, http.StatusNotFound, "no updater configured")
		return
	}
	if a.updater.Updating() {
		writeErr(w, http.StatusConflict, "an update is already running")
		return
	}
	// The reverse of the install guard: an update while a plugin install or
	// uninstall is writing into the game tree would clobber its files.
	if targets := a.jobs.RunningTargets(); len(targets) > 0 {
		writeErr(w, http.StatusConflict, "a plugin job is running ("+strings.Join(targets, ", ")+") — wait for it to finish before updating the server")
		return
	}
	a.updater.SetUpdating(true)
	job, err := a.jobs.Start("update", "cs2", "CS2 server", func(ctx context.Context, progress func(Progress)) (*InstallResult, error) {
		defer a.updater.SetUpdating(false)
		err := a.updater.Run(ctx, func(step string) {
			progress(Progress{Step: step})
		})
		if err != nil {
			return nil, err
		}
		// An update changes the binary the game unit launches, so a restart
		// is implied for a server that was online — Run already did it.
		return &InstallResult{ID: "cs2", RequiresRestart: false}, nil
	})
	if err != nil {
		a.updater.SetUpdating(false)
		var busy *ErrBusy
		if errors.As(err, &busy) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// action performs a lifecycle action and reports the unit's real state.
// A 200 here means "the unit reached the requested state"; a systemctl exit
// code alone never did.
func (a *API) action(name Action) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res := a.server.Control(r.Context(), name)
		code := http.StatusOK
		if res.Failed {
			code = http.StatusConflict
		}
		writeJSON(w, code, res)
	}
}

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 120
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	lines, err := a.server.Logs(n)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (a *API) handleRCONCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.server.DiagnoseRCON())
}

func (a *API) handleRCONRepair(w http.ResponseWriter, r *http.Request) {
	applied, res, err := a.server.RepairRCON(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if applied == nil {
		applied = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied": applied,
		"result":  res,
		"rcon":    a.server.DiagnoseRCON(),
	})
}

type execRequest struct {
	Command string `json:"command"`
}

func (a *API) handleExec(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	out, err := a.server.Exec(r.Context(), req.Command)
	if err != nil {
		// A truncated answer is worth showing, flagged as incomplete: half a
		// cvarlist is useful, silently passing it off as the whole answer is not.
		if out != "" && errors.Is(err, rcon.ErrTruncated) {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "output": out, "truncated": true, "note": err.Error(),
			})
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": out})
}

func (a *API) handleMaps(w http.ResponseWriter, r *http.Request) {
	maps, err := a.server.Maps()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"maps": maps})
}

// handleCosmetics serves the knife/glove/agent catalogs (with local image
// paths) so the panel can render pickers without hardcoding game data.
func (a *API) handleCosmetics(w http.ResponseWriter, r *http.Request) {
	gloves := Gloves()
	tAgents, ctAgents := Agents()
	writeJSON(w, http.StatusOK, map[string]any{
		"gloves":       gloves,
		"agents_t":     tAgents,
		"agents_ct":    ctAgents,
		"weapons":      Weapons(),
		"sync_enabled": a.loadout.WPEnabled(),
	})
}

func (a *API) handleMapChange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Map   string `json:"map"`
		Force bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	var err error
	if req.Force {
		err = a.server.ChangeMapForce(req.Map)
	} else {
		err = a.server.ChangeMap(r.Context(), req.Map)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The map change must outlive the session it was made in: record it so
	// the next server start launches this map instead of the unit default.
	// Workshop ids cannot be recorded — the unit's +map argument has no way
	// to express host_workshop_map, so those changes stay session-only.
	// A persist failure leaves the running map correct but the restart
	// regression in place — worth reporting, not worth undoing the change.
	warn := ""
	if reWorkshopID.MatchString(req.Map) {
		warn = "workshop map changes do not survive a restart"
	} else if perr := WriteMapEnv(a.cfg.MapEnvFile, req.Map); perr != nil {
		warn = "map change applied, but it will not survive a restart: " + perr.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "map": req.Map, "warning": warn})
}

func (a *API) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := a.server.ManagedSettings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if settings == nil {
		settings = []cs2Setting{}
	}
	out := map[string]any{"settings": settings}
	// A duplicate managed block means the panel is not showing what the server
	// runs. Saying so beats letting the operator wonder why their change had no
	// effect.
	if warn := a.server.ManagedBlockWarning(); warn != "" {
		out["warning"] = warn
	}
	writeJSON(w, http.StatusOK, out)
}

type settingsRequest struct {
	Settings []cs2Setting `json:"settings"`
}

// maxSettings bounds one settings save. server.cfg is a config file, not a data
// store: an unbounded list could be used to grow it without limit.
const maxSettings = 200

// maxSettingValue is generous (a workshop collection id list or an MOTD URL is
// long) but finite.
const (
	maxSettingValue   = 1024
	maxSettingComment = 200
)

func (a *API) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if len(req.Settings) > maxSettings {
		writeErr(w, http.StatusBadRequest, "too many settings")
		return
	}
	// Validate at the boundary, not at render time. Rendering strips control
	// characters, so a rejected value used to be silently altered instead —
	// the panel then showed a setting the file did not contain.
	seen := make(map[string]struct{}, len(req.Settings))
	for _, s := range req.Settings {
		if !reCvarName.MatchString(s.Name) {
			writeErr(w, http.StatusBadRequest, "invalid cvar name: "+s.Name)
			return
		}
		if _, dup := seen[strings.ToLower(s.Name)]; dup {
			// The last line would win in the engine and in the file, while the
			// panel showed the first: a silent disagreement.
			writeErr(w, http.StatusBadRequest, "duplicate cvar: "+s.Name)
			return
		}
		seen[strings.ToLower(s.Name)] = struct{}{}
		if len(s.Value) > maxSettingValue {
			writeErr(w, http.StatusBadRequest, "value too long for "+s.Name)
			return
		}
		if len(s.Comment) > maxSettingComment {
			writeErr(w, http.StatusBadRequest, "comment too long for "+s.Name)
			return
		}
		if hasCfgControlChar(s.Value) || hasCfgControlChar(s.Comment) {
			writeErr(w, http.StatusBadRequest, "invalid characters in "+s.Name)
			return
		}
	}
	if err := a.server.ApplyManagedSettings(r.Context(), req.Settings); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"ok": true}
	if warn := a.server.ManagedBlockWarning(); warn != "" {
		out["warning"] = warn
	}
	writeJSON(w, http.StatusOK, out)
}

// hasCfgControlChar reports characters that cannot appear in a config line.
// A newline is the dangerous one — it turns one setting into two config
// statements — but a NUL or an escape would also make the file unreadable in a
// way the operator cannot see.
func hasCfgControlChar(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func (a *API) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if len(req.Password) > 128 {
		writeErr(w, http.StatusBadRequest, "password too long")
		return
	}
	req.Password = strings.TrimSpace(req.Password)
	lockedNow, err := a.server.SetPassword(r.Context(), req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// locked_now tells the panel whether the map reload engaged: false means
	// the server is offline (or unreachable) and the password applies at the
	// next boot rather than immediately.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "set": req.Password != "", "locked_now": lockedNow})
}

func (a *API) handlePlugins(w http.ResponseWriter, r *http.Request) {
	cat, err := a.inst.Catalog()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": cat})
}

func (a *API) handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Force bool `json:"force"`
		// Async starts a background job and returns immediately with its id.
		// The panel uses this so a multi-minute download never rides on a
		// single HTTP request (which any reverse proxy is free to cut).
		Async bool `json:"async"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional
	entry, ok := Find(a.inst.catalog, id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown plugin "+id)
		return
	}
	// A server update rewrites the game tree the installer extracts into;
	// letting both run leaves the winner's files half-written by the loser.
	if a.updater != nil && a.updater.Updating() {
		writeErr(w, http.StatusConflict, "the CS2 server is being updated right now — wait for it to finish before installing plugins")
		return
	}
	if !req.Async {
		res, err := a.inst.Install(r.Context(), id, req.Force)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	force := req.Force
	job, err := a.jobs.Start("install", id, entry.Name, func(ctx context.Context, progress func(Progress)) (*InstallResult, error) {
		res, err := a.inst.InstallProgress(ctx, id, force, func(p Progress) { progress(p) })
		if err != nil {
			return nil, err
		}
		return &res, nil
	})
	if err != nil {
		var busy *ErrBusy
		if errors.As(err, &busy) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (a *API) handlePluginUninstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if a.updater != nil && a.updater.Updating() {
		writeErr(w, http.StatusConflict, "the CS2 server is being updated right now — wait for it to finish before uninstalling plugins")
		return
	}
	// An uninstall while an install is in flight deletes files the extraction is
	// still writing: the plugin ends up half-present and its recorded manifest
	// no longer describes what is on disk. The dependency check below cannot see
	// this, because the dependency is not installed *yet*.
	for _, target := range a.jobs.RunningTargets() {
		if target == id {
			writeErr(w, http.StatusConflict, displayName(a.inst.catalog, id)+" is being installed right now — wait for that to finish first")
			return
		}
		if dependsOn(a.inst.catalog, target, id) {
			writeErr(w, http.StatusConflict, displayName(a.inst.catalog, target)+" is being installed right now and needs "+displayName(a.inst.catalog, id)+" — wait for that to finish first")
			return
		}
	}
	if err := a.inst.Uninstall(id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not installed")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs := a.jobs.List()
	a.markRestarts(jobs)
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (a *API) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, ok := a.jobs.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown job")
		return
	}
	a.markRestarts([]Job{job})
	writeJSON(w, http.StatusOK, job)
}

// markRestarts sets RestartObserved on finished installs whose "restart the
// server to load it" hint is stale: the game unit has entered its current run
// after the install finished, so the restart already happened. The hint
// otherwise survives every page load for the whole job retention window and
// tells the operator to do something they already did.
func (a *API) markRestarts(jobs []Job) {
	var boot time.Time
	haveBoot := false
	for i := range jobs {
		j := &jobs[i]
		if j.Status != JobDone || j.Kind != "install" || j.Result == nil || !j.Result.RequiresRestart {
			continue
		}
		if !haveBoot {
			// One cheap systemctl read no matter how many jobs there are;
			// skipped entirely when no job carries the hint.
			uptime, ok := a.server.sysd.UptimeSeconds()
			if !ok || uptime <= 0 {
				return
			}
			boot = time.Now().Add(-time.Duration(uptime * float64(time.Second)))
			haveBoot = true
		}
		if boot.After(j.Finished) {
			j.RestartObserved = true
		}
	}
}

func (a *API) handleGetWhitelist(w http.ResponseWriter, r *http.Request) {
	ids, err := a.wh.Read()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ids == nil {
		ids = []string{}
	}
	enabled, err := a.wh.Enabled()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// "installed" lets the access page show the whitelist card only when the
	// feature exists on this server. The id is the catalog entry's ID, not the
	// GitHub repo name: this used to query "mm-cs2whitelist" (the repo), which
	// is never what Install records, so the access page kept telling an
	// operator who had just installed the plugin that it was not installed.
	writeJSON(w, http.StatusOK, map[string]any{
		"steamids":  ids,
		"enabled":   enabled,
		"installed": a.inst.IsInstalled("cs2whitelist"),
	})
}

// handlePutWhitelistEnabled flips enforcement in the plugin's core.cfg and
// pushes the same value to the running server.
//
// core.cfg is only parsed when the plugin loads (AllPluginsLoaded), so writing
// the file alone changed nothing until the next server restart — the Access
// page's toggle appeared to work and the server kept enforcing the old value.
// The live switch is the plugin's own cvar, and both are best-effort because
// RCON may be down or the plugin may not be installed at all.
func (a *API) handlePutWhitelistEnabled(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := a.wh.SetEnabled(req.Enabled); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.wh.PushLive(a.server, req.Enabled)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": req.Enabled})
}

func (a *API) handlePutWhitelist(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SteamIDs []string `json:"steamids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	ids, err := a.wh.Apply(req.SteamIDs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if ids == nil {
		ids = []string{}
	}
	// The plugin holds the file in memory plus a per-map decision cache, so a
	// reload alone would still reject a player it had already turned away.
	a.wh.ReloadLive(a.server)
	writeJSON(w, http.StatusOK, map[string]any{"steamids": ids})
}

// --- helpers ----------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

// --- loadout + plugin config handlers -----------------------------------

func (a *API) handleGetLoadout(w http.ResponseWriter, r *http.Request) {
	lo, err := a.loadout.Get(r.PathValue("steamid"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"loadout": lo, "sync_enabled": a.loadout.WPEnabled()})
}

func (a *API) handlePutLoadout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Loadout Loadout `json:"loadout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	var warning string
	if err := a.loadout.Set(r.PathValue("steamid"), req.Loadout); err != nil {
		// A failed WeaponPaints sync is not a failed save: the selection is
		// already stored, and reporting 400 made the panel tell the player their
		// loadout was rejected when it had in fact been kept.
		msg, ok := asWarning(err)
		if !ok {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		warning = msg
	}
	// The panel needs to know whether the selection can actually reach the
	// game: without WeaponPaints' database the loadout is only stored locally,
	// and telling the player "it applies when you reconnect" would be a lie.
	out := map[string]any{"ok": true, "sync_enabled": a.loadout.WPEnabled()}
	if warning != "" {
		out["warning"] = warning
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) handleGetPluginConfig(w http.ResponseWriter, r *http.Request) {
	raw, exists, err := a.inst.GetPluginConfig(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out := map[string]any{"exists": exists}
	if raw == nil {
		out["json"] = map[string]any{}
	} else {
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			writeErr(w, http.StatusInternalServerError, "config is not valid json: "+err.Error())
			return
		}
		out["json"] = doc
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) handlePutPluginConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSON map[string]any `json:"json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	b, err := json.Marshal(req.JSON)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.inst.PutPluginConfig(r.PathValue("id"), b); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
