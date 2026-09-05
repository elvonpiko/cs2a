package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// AgentClient is the typed client for the agent's loopback API.
type AgentClient struct {
	Base  string // e.g. http://127.0.0.1:8100
	Token string
	HTTP  *http.Client
}

// Per-operation deadlines. The agent runs long jobs synchronously (a plugin
// install downloads tens of megabytes and unpacks them; a map change stalls
// the game server), so a single short client timeout is what produced the
// "timeout" errors in the UI. Each call now picks a budget that matches the
// work instead.
const (
	// quickTimeout covers reads and cheap writes (status, settings, whitelist).
	quickTimeout = 15 * time.Second
	// actionTimeout covers systemd start/stop/restart, RCON commands and map
	// changes: the game server can be busy for a while.
	actionTimeout = 90 * time.Second
	// installTimeout covers plugin download + extraction (CounterStrikeSharp
	// with-runtime is ~50 MB and expands to a few hundred).
	installTimeout = 20 * time.Minute
)

// NewAgentClient builds a client with no global timeout: every call sets its
// own deadline via context so slow operations are not cut short while fast
// ones still fail quickly.
func NewAgentClient(base, token string) *AgentClient {
	return &AgentClient{
		Base:  strings.TrimRight(base, "/"),
		Token: token,
		HTTP: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				ResponseHeaderTimeout: 0, // bounded per call by context
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       60 * time.Second,
			},
		},
	}
}

// APIError is a structured agent error.
type APIError struct {
	Status  int
	Message string
}

// Error returns the agent's message as-is. It deliberately does not prepend
// "agent: 400:" — the agent's messages are written for the operator, and the
// transport status only made the flash text harder to read (the user saw
// "Map change failed: agent: 400: rcon: dial 127.0.0.1:27015: …").
func (e *APIError) Error() string {
	if e.Message == "" {
		return http.StatusText(e.Status)
	}
	return e.Message
}

func (c *AgentClient) do(ctx context.Context, method, path string, body, out any) error {
	return c.doTimeout(ctx, method, path, body, out, quickTimeout)
}

// doTimeout performs a request with an explicit per-operation deadline. When
// the caller's context already has an earlier deadline, that one wins.
func (c *AgentClient) doTimeout(ctx context.Context, method, path string, body, out any, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return friendlyErr(err, timeout)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		msg := http.StatusText(resp.StatusCode)
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err := json.Unmarshal(raw, &e); err == nil && e.Error != "" {
			msg = e.Error
		}
		apiErr := &APIError{Status: resp.StatusCode, Message: msg}
		// A 409 carries a structured ActionResult (a unit that would not
		// start), which the caller needs in full to show the journal.
		if out != nil && resp.StatusCode == http.StatusConflict {
			if err := json.Unmarshal(raw, out); err == nil {
				return apiErr
			}
		}
		return apiErr
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("agent: decode %s: %w", path, err)
		}
	}
	return nil
}

// friendlyErr turns transport failures into messages an operator can act on.
func friendlyErr(err error, timeout time.Duration) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("the agent did not answer within %s — it may still be working; reload in a moment", timeout.Round(time.Second))
	case errors.Is(err, syscall.ECONNREFUSED):
		return errors.New("cannot reach the cs2a agent — check it is running with: systemctl status cs2a-agent")
	case errors.Is(err, context.Canceled):
		return errors.New("the request was cancelled")
	default:
		return fmt.Errorf("cannot reach the cs2a agent: %w", err)
	}
}

// ServerStatus mirrors the agent's FullStatus payload.
type ServerStatus struct {
	Service struct {
		Active        bool    `json:"active"`
		Sub           string  `json:"sub"`
		Enabled       bool    `json:"enabled"`
		UptimeSeconds float64 `json:"uptime_seconds"`
	} `json:"service"`
	Info *struct {
		Name    string `json:"name"`
		Map     string `json:"map"`
		Players int    `json:"players"`
		Max     int    `json:"max"`
		Bots    int    `json:"bots"`
	} `json:"info,omitempty"`
	Rcon *struct {
		Hostname string `json:"hostname"`
		Map      string `json:"map"`
		Players  []struct {
			UserID    string `json:"user_id"`
			Name      string `json:"name"`
			SteamID   string `json:"steam_id"`
			Addr      string `json:"addr"`
			Connected string `json:"connected"`
			Ping      int    `json:"ping"`
			Loss      int    `json:"loss"`
			State     string `json:"state"`
			Bot       bool   `json:"bot"`
		} `json:"players"`
		Humans int `json:"humans"`
		Bots   int `json:"bots"`
		Max    int `json:"max"`
	} `json:"rcon,omitempty"`
	Note string         `json:"note"`
	Diag *RCONDiagnosis `json:"diag,omitempty"`
}

// RCONDiagnosis mirrors the agent's RCON diagnosis.
type RCONDiagnosis struct {
	OK               bool   `json:"ok"`
	Addr             string `json:"addr"`
	Reason           string `json:"reason"`
	Fix              string `json:"fix"`
	Repairable       bool   `json:"repairable"`
	MissingUserCon   bool   `json:"missing_usercon"`
	MissingPassword  bool   `json:"missing_password"`
	SuggestedAddr    string `json:"suggested_addr"`
	PasswordMismatch bool   `json:"password_mismatch"`
}

// Status fetches the composed server status.
func (c *AgentClient) Status(ctx context.Context) (*ServerStatus, error) {
	var st ServerStatus
	if err := c.do(ctx, http.MethodGet, "/api/v1/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ActionResult mirrors the agent's verified lifecycle result.
type ActionResult struct {
	Action  string   `json:"action"`
	Active  bool     `json:"active"`
	Sub     string   `json:"sub"`
	Message string   `json:"message"`
	Failed  bool     `json:"failed"`
	Log     []string `json:"log"`
}

// ServerAction performs start/stop/restart and reports what actually happened.
// The agent verifies the unit settled, so a nil error here means the server
// really reached the requested state.
func (c *AgentClient) ServerAction(ctx context.Context, action string) (*ActionResult, error) {
	var res ActionResult
	err := c.doTimeout(ctx, http.MethodPost, "/api/v1/server/"+action, nil, &res, actionTimeout)
	if err != nil {
		if res.Message != "" {
			// A 409 with a structured body: the message and journal are the
			// answer, not the HTTP status.
			return &res, errors.New(res.Message)
		}
		return nil, err
	}
	return &res, nil
}

// Logs fetches the tail of the game server's journal.
func (c *AgentClient) Logs(ctx context.Context, n int) ([]string, error) {
	var out struct {
		Lines []string `json:"lines"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/server/logs?n=%d", n), nil, &out); err != nil {
		return nil, err
	}
	return out.Lines, nil
}

// RCONCheck asks the agent to diagnose RCON reachability.
func (c *AgentClient) RCONCheck(ctx context.Context) (*RCONDiagnosis, error) {
	var d RCONDiagnosis
	if err := c.do(ctx, http.MethodGet, "/api/v1/server/rcon-check", nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// RCONRepair applies the fixes the diagnosis identified and restarts the game
// server. It returns what was changed and the resulting state.
func (c *AgentClient) RCONRepair(ctx context.Context) ([]string, *ActionResult, *RCONDiagnosis, error) {
	var out struct {
		Applied []string       `json:"applied"`
		Result  ActionResult   `json:"result"`
		Rcon    *RCONDiagnosis `json:"rcon"`
	}
	if err := c.doTimeout(ctx, http.MethodPost, "/api/v1/server/rcon-repair", nil, &out, actionTimeout); err != nil {
		return nil, nil, nil, err
	}
	return out.Applied, &out.Result, out.Rcon, nil
}

// Exec runs a console command. A truncated answer comes back as the partial
// output with the agent's note appended, so a caller cannot mistake half a
// cvarlist for the whole thing.
func (c *AgentClient) Exec(ctx context.Context, command string) (string, error) {
	var out struct {
		Output    string `json:"output"`
		Truncated bool   `json:"truncated"`
		Note      string `json:"note"`
	}
	if err := c.doTimeout(ctx, http.MethodPost, "/api/v1/server/exec", map[string]string{"command": command}, &out, actionTimeout); err != nil {
		return "", err
	}
	if out.Truncated && out.Note != "" {
		return out.Output + "\n\n[" + out.Note + "]", nil
	}
	return out.Output, nil
}

// Maps lists installed maps.
func (c *AgentClient) Maps(ctx context.Context) ([]string, error) {
	var out struct {
		Maps []string `json:"maps"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/maps", nil, &out); err != nil {
		return nil, err
	}
	return out.Maps, nil
}

// ChangeMap switches the map.
func (c *AgentClient) ChangeMap(ctx context.Context, mapName string, force bool) error {
	return c.doTimeout(ctx, http.MethodPost, "/api/v1/map", map[string]any{"map": mapName, "force": force}, nil, actionTimeout)
}

// Setting is one managed server.cfg cvar.
type Setting struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Comment string `json:"comment,omitempty"`
	// Bare marks a value-less line the operator wrote inside the managed block
	// (the engine treats it as a query). It must survive a round trip through
	// the panel, or saving any setting rewrites it as `name ""` — which sets
	// that cvar to 0.
	Bare bool `json:"bare,omitempty"`
}

// Settings returns managed settings plus a warning when server.cfg is in a
// state cs2a can write to but not fully control (today: a duplicate managed
// block, whose second copy overrides everything the panel writes).
func (c *AgentClient) Settings(ctx context.Context) ([]Setting, string, error) {
	var out struct {
		Settings []Setting `json:"settings"`
		Warning  string    `json:"warning"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/settings", nil, &out); err != nil {
		return nil, "", err
	}
	return out.Settings, out.Warning, nil
}

// PutSettings writes managed settings and returns the same warning.
func (c *AgentClient) PutSettings(ctx context.Context, settings []Setting) (string, error) {
	var out struct {
		Warning string `json:"warning"`
	}
	if err := c.do(ctx, http.MethodPut, "/api/v1/settings", map[string]any{"settings": settings}, &out); err != nil {
		return "", err
	}
	return out.Warning, nil
}

// SetPassword sets/clears sv_password.
func (c *AgentClient) SetPassword(ctx context.Context, pw string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/password", map[string]string{"password": pw}, nil)
}

// PluginEntry is one catalog item as returned by the agent.
type PluginEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      string   `json:"author"`
	Kind        string   `json:"kind"`
	Requires    []string `json:"requires"`
	Homepage    string   `json:"homepage"`
	// ConfigPath is non-empty when the plugin has a config file the panel can
	// edit. Every card used to advertise a config editor unconditionally.
	ConfigPath       string `json:"config_path"`
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installed_version"`
	// RequiredBy names installed components that depend on this one; while it
	// is non-empty, uninstalling would break them.
	RequiredBy []string `json:"required_by"`
}

// Plugins lists the catalog with installed annotations.
func (c *AgentClient) Plugins(ctx context.Context) ([]PluginEntry, error) {
	var out struct {
		Plugins []PluginEntry `json:"plugins"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/plugins", nil, &out); err != nil {
		return nil, err
	}
	return out.Plugins, nil
}

// InstallResult mirrors the agent install response.
type InstallResult struct {
	ID              string `json:"id"`
	Version         string `json:"version"`
	RequiresRestart bool   `json:"requires_restart"`
	InstalledDeps   bool   `json:"installed_deps"`
	// Warning is a non-fatal problem the operator should still see (a file the
	// release did not ship, ownership that could not be aligned).
	Warning string `json:"warning"`
}

// Install installs a plugin. Downloads run inside the request, so this uses
// the long install budget.
func (c *AgentClient) Install(ctx context.Context, id string, force bool) (*InstallResult, error) {
	var res InstallResult
	if err := c.doTimeout(ctx, http.MethodPost, "/api/v1/plugins/"+id+"/install", map[string]bool{"force": force}, &res, installTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// Uninstall removes a plugin.
func (c *AgentClient) Uninstall(ctx context.Context, id string) error {
	return c.doTimeout(ctx, http.MethodDelete, "/api/v1/plugins/"+id, nil, nil, actionTimeout)
}

// WhitelistState is the agent's whitelist file plus its enforcement switch.
type WhitelistState struct {
	SteamIDs []string `json:"steamids"`
	Enabled  bool     `json:"enabled"`
}

// WhitelistState returns the whitelist entries and whether the plugin is
// enforcing them.
func (c *AgentClient) WhitelistState(ctx context.Context) (*WhitelistState, error) {
	var out WhitelistState
	if err := c.do(ctx, http.MethodGet, "/api/v1/whitelist", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Whitelist returns the agent-side whitelist file contents.
func (c *AgentClient) Whitelist(ctx context.Context) ([]string, error) {
	st, err := c.WhitelistState(ctx)
	if err != nil {
		return nil, err
	}
	return st.SteamIDs, nil
}

// PutWhitelist replaces the whitelist.
func (c *AgentClient) PutWhitelist(ctx context.Context, ids []string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/whitelist", map[string]any{"steamids": ids}, nil)
}

// SetWhitelistEnabled toggles whitelist enforcement in the plugin's config.
func (c *AgentClient) SetWhitelistEnabled(ctx context.Context, on bool) error {
	return c.do(ctx, http.MethodPut, "/api/v1/whitelist/enabled", map[string]bool{"enabled": on}, nil)
}

// Job mirrors the agent's async job record.
type Job struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Target   string         `json:"target"`
	Label    string         `json:"label"`
	Status   string         `json:"status"` // running | done | failed
	Step     string         `json:"step"`
	Message  string         `json:"message"`
	Result   *InstallResult `json:"result"`
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished"`
}

// Running reports whether the job is still in flight.
func (j *Job) Running() bool { return j.Status == "running" }

// InstallAsync starts a background install on the agent and returns the job.
// The HTTP request itself returns immediately, so no proxy or client timeout
// can interrupt a long download.
func (c *AgentClient) InstallAsync(ctx context.Context, id string, force bool) (*Job, error) {
	var job Job
	if err := c.do(ctx, http.MethodPost, "/api/v1/plugins/"+id+"/install",
		map[string]bool{"force": force, "async": true}, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// JobStatus fetches one job.
func (c *AgentClient) JobStatus(ctx context.Context, id string) (*Job, error) {
	var job Job
	if err := c.do(ctx, http.MethodGet, "/api/v1/jobs/"+id, nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// Jobs lists retained jobs (running first by recency).
func (c *AgentClient) Jobs(ctx context.Context) ([]Job, error) {
	var out struct {
		Jobs []Job `json:"jobs"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/jobs", nil, &out); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

// PlayerLoadout is the agent-side loadout for one steamid.
type PlayerLoadout struct {
	KnifeT      string `json:"knife_t"`
	KnifeCT     string `json:"knife_ct"`
	GlovesT     string `json:"gloves_t,omitempty"`
	GlovesCT    string `json:"gloves_ct,omitempty"`
	AgentT      string `json:"agent_t,omitempty"`
	AgentCT     string `json:"agent_ct,omitempty"`
	SyncEnabled bool   `json:"-"`
}

// CosmeticEntry is one selectable glove/agent for the loadout UI.
type CosmeticEntry struct {
	Defindex int    `json:"defindex,omitempty"`
	Paint    int    `json:"paint,omitempty"`
	Model    string `json:"model,omitempty"`
	Name     string `json:"name"`
	Image    string `json:"image,omitempty"`
}

// Cosmetics fetches the glove/agent catalogs from the agent.
func (c *AgentClient) Cosmetics(ctx context.Context) (gloves, agentsT, agentsCT []CosmeticEntry, err error) {
	var out struct {
		Gloves   []CosmeticEntry `json:"gloves"`
		AgentsT  []CosmeticEntry `json:"agents_t"`
		AgentsCT []CosmeticEntry `json:"agents_ct"`
	}
	if err = c.do(ctx, http.MethodGet, "/api/v1/cosmetics", nil, &out); err != nil {
		return nil, nil, nil, err
	}
	return out.Gloves, out.AgentsT, out.AgentsCT, nil
}

// GetLoadout fetches a player's loadout from the agent store.
func (c *AgentClient) GetLoadout(ctx context.Context, steamid string) (*PlayerLoadout, error) {
	var out struct {
		Loadout struct {
			KnifeT   string `json:"knife_t"`
			KnifeCT  string `json:"knife_ct"`
			GlovesT  string `json:"gloves_t"`
			GlovesCT string `json:"gloves_ct"`
			AgentT   string `json:"agent_t"`
			AgentCT  string `json:"agent_ct"`
		} `json:"loadout"`
		SyncEnabled bool `json:"sync_enabled"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/loadout/"+steamid, nil, &out); err != nil {
		return nil, err
	}
	return &PlayerLoadout{
		KnifeT:      out.Loadout.KnifeT,
		KnifeCT:     out.Loadout.KnifeCT,
		GlovesT:     out.Loadout.GlovesT,
		GlovesCT:    out.Loadout.GlovesCT,
		AgentT:      out.Loadout.AgentT,
		AgentCT:     out.Loadout.AgentCT,
		SyncEnabled: out.SyncEnabled,
	}, nil
}

// PutLoadout pushes a player's cosmetic selection to the agent (which syncs
// to WeaponPaints' MySQL when configured). It reports whether that sync is
// active: without it the selection is only stored locally and will not show up
// in game, which the player has to be told. A non-empty warning means the save
// succeeded but the sync did not, and says why.
func (c *AgentClient) PutLoadout(ctx context.Context, steamid string, lo *PlayerLoadout) (syncEnabled bool, warning string, err error) {
	var out struct {
		SyncEnabled bool   `json:"sync_enabled"`
		Warning     string `json:"warning"`
	}
	err = c.do(ctx, http.MethodPut, "/api/v1/loadout/"+steamid,
		map[string]any{"loadout": map[string]any{
			"knife_t": lo.KnifeT, "knife_ct": lo.KnifeCT,
			"gloves_t": lo.GlovesT, "gloves_ct": lo.GlovesCT,
			"agent_t": lo.AgentT, "agent_ct": lo.AgentCT,
		}}, &out)
	return out.SyncEnabled, out.Warning, err
}

// PluginConfig fetches a plugin's config JSON (pretty-printed).
// Returns nil bytes when the file does not exist yet.
func (c *AgentClient) PluginConfig(ctx context.Context, id string) ([]byte, error) {
	var out struct {
		Exists bool            `json:"exists"`
		JSON   json.RawMessage `json:"json"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/plugins/"+id+"/config", nil, &out); err != nil {
		return nil, err
	}
	if !out.Exists || len(out.JSON) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, out.JSON, "", "  "); err != nil {
		return out.JSON, nil
	}
	return buf.Bytes(), nil
}

// SavePluginConfig validates and saves a plugin config JSON.
func (c *AgentClient) SavePluginConfig(ctx context.Context, id, jsonBody string) error {
	var doc any
	dec := json.NewDecoder(strings.NewReader(jsonBody))
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("config is not valid JSON: %w", err)
	}
	obj, ok := doc.(map[string]any)
	if !ok {
		return fmt.Errorf("config must be a JSON object")
	}
	return c.do(ctx, http.MethodPut, "/api/v1/plugins/"+id+"/config", map[string]any{"json": obj}, nil)
}
