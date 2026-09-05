package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// API is the agent's HTTP surface. It binds to loopback only; the panel is
// the sole intended client, authenticating with the shared bearer token.
type API struct {
	cfg     Config
	server  *Server
	wh      *Whitelist
	inst    *Installer
	loadout *LoadoutStore
}

// NewAPI wires the HTTP API.
func NewAPI(cfg Config, srv *Server, wh *Whitelist, inst *Installer, loadout *LoadoutStore) *API {
	return &API{cfg: cfg, server: srv, wh: wh, inst: inst, loadout: loadout}
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
	mux.HandleFunc("POST /api/v1/server/start", auth(a.action("start")))
	mux.HandleFunc("POST /api/v1/server/stop", auth(a.action("stop")))
	mux.HandleFunc("POST /api/v1/server/restart", auth(a.action("restart")))
	mux.HandleFunc("POST /api/v1/server/exec", auth(a.handleExec))
	mux.HandleFunc("GET /api/v1/maps", auth(a.handleMaps))
	mux.HandleFunc("GET /api/v1/cosmetics", auth(a.handleCosmetics))
	mux.HandleFunc("POST /api/v1/map", auth(a.handleMapChange))
	mux.HandleFunc("GET /api/v1/settings", auth(a.handleGetSettings))
	mux.HandleFunc("PUT /api/v1/settings", auth(a.handlePutSettings))
	mux.HandleFunc("PUT /api/v1/password", auth(a.handlePassword))
	mux.HandleFunc("GET /api/v1/plugins", auth(a.handlePlugins))
	mux.HandleFunc("POST /api/v1/plugins/{id}/install", auth(a.handlePluginInstall))
	mux.HandleFunc("DELETE /api/v1/plugins/{id}", auth(a.handlePluginUninstall))
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
		tok := r.Header.Get("Authorization")
		tok = strings.TrimPrefix(tok, "Bearer ")
		if tok == "" || subtleConstantCompare(tok, a.cfg.Token) != true {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func subtleConstantCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// --- handlers ---------------------------------------------------------

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "cs2a-agent"})
}

func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.server.Status(r.Context()))
}

func (a *API) action(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var err error
		switch name {
		case "start":
			err = a.server.Start()
		case "stop":
			err = a.server.Stop()
		case "restart":
			err = a.server.Restart()
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": name})
	}
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "map": req.Map})
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
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings})
}

type settingsRequest struct {
	Settings []cs2Setting `json:"settings"`
}

func (a *API) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	for _, s := range req.Settings {
		if !reCvarName.MatchString(s.Name) {
			writeErr(w, http.StatusBadRequest, "invalid cvar name: "+s.Name)
			return
		}
	}
	if err := a.server.ApplyManagedSettings(r.Context(), req.Settings); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	if err := a.server.SetPassword(r.Context(), req.Password); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // body optional
	res, err := a.inst.Install(r.Context(), id, req.Force)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) handlePluginUninstall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	writeJSON(w, http.StatusOK, map[string]any{"steamids": ids, "enabled": enabled})
}

// handlePutWhitelistEnabled flips enforcement in the plugin's core.cfg and
// asks the plugin to reload it (best effort — RCON may be down).
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
	_ = a.server.ExecQuiet("wl_reload")
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
	// the plugin caches the file; ask it to reload (no-op when not installed)
	_ = a.server.ExecQuiet("wl_reload")
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
	if err := a.loadout.Set(r.PathValue("steamid"), req.Loadout); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
