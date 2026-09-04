package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// AgentClient is the typed client for the agent's loopback API.
type AgentClient struct {
	Base  string // e.g. http://127.0.0.1:8100
	Token string
	HTTP  *http.Client
}

// NewAgentClient builds a client with sane defaults.
func NewAgentClient(base, token string) *AgentClient {
	return &AgentClient{
		Base:  strings.TrimRight(base, "/"),
		Token: token,
		HTTP:  &http.Client{Timeout: 15 * time.Second},
	}
}

// APIError is a structured agent error.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("agent: %d: %s", e.Status, e.Message)
}

func (c *AgentClient) do(ctx context.Context, method, path string, body, out any) error {
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
		return fmt.Errorf("agent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		msg := http.StatusText(resp.StatusCode)
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e); err == nil && e.Error != "" {
			msg = e.Error
		}
		return &APIError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("agent: decode %s: %w", path, err)
		}
	}
	return nil
}

// Health pings the agent.
func (c *AgentClient) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/api/v1/health", nil, nil)
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
			Connected string `json:"connected"`
			Ping      int    `json:"ping"`
			State     string `json:"state"`
		} `json:"players"`
		Humans int `json:"humans"`
		Bots   int `json:"bots"`
		Max    int `json:"max"`
	} `json:"rcon,omitempty"`
	Note string `json:"note"`
}

// Status fetches the composed server status.
func (c *AgentClient) Status(ctx context.Context) (*ServerStatus, error) {
	var st ServerStatus
	if err := c.do(ctx, http.MethodGet, "/api/v1/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ServerAction performs start/stop/restart.
func (c *AgentClient) ServerAction(ctx context.Context, action string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/server/"+action, nil, nil)
}

// Exec runs a console command.
func (c *AgentClient) Exec(ctx context.Context, command string) (string, error) {
	var out struct {
		Output string `json:"output"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/server/exec", map[string]string{"command": command}, &out); err != nil {
		return "", err
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
	return c.do(ctx, http.MethodPost, "/api/v1/map", map[string]any{"map": mapName, "force": force}, nil)
}

// Setting is one managed server.cfg cvar.
type Setting struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Comment string `json:"comment,omitempty"`
}

// Settings returns managed settings.
func (c *AgentClient) Settings(ctx context.Context) ([]Setting, error) {
	var out struct {
		Settings []Setting `json:"settings"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/settings", nil, &out); err != nil {
		return nil, err
	}
	return out.Settings, nil
}

// PutSettings writes managed settings.
func (c *AgentClient) PutSettings(ctx context.Context, settings []Setting) error {
	return c.do(ctx, http.MethodPut, "/api/v1/settings", map[string]any{"settings": settings}, nil)
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
}

// Install installs a plugin.
func (c *AgentClient) Install(ctx context.Context, id string, force bool) (*InstallResult, error) {
	var res InstallResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/plugins/"+id+"/install", map[string]bool{"force": force}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Uninstall removes a plugin.
func (c *AgentClient) Uninstall(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/plugins/"+id, nil, nil)
}

// Whitelist returns the agent-side whitelist file contents.
func (c *AgentClient) Whitelist(ctx context.Context) ([]string, error) {
	var out struct {
		SteamIDs []string `json:"steamids"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/whitelist", nil, &out); err != nil {
		return nil, err
	}
	return out.SteamIDs, nil
}

// PutWhitelist replaces the whitelist.
func (c *AgentClient) PutWhitelist(ctx context.Context, ids []string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/whitelist", map[string]any{"steamids": ids}, nil)
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
// to WeaponPaints' MySQL when configured).
func (c *AgentClient) PutLoadout(ctx context.Context, steamid string, lo *PlayerLoadout) error {
	return c.do(ctx, http.MethodPut, "/api/v1/loadout/"+steamid,
		map[string]any{"loadout": map[string]any{
			"knife_t": lo.KnifeT, "knife_ct": lo.KnifeCT,
			"gloves_t": lo.GlovesT, "gloves_ct": lo.GlovesCT,
			"agent_t": lo.AgentT, "agent_ct": lo.AgentCT,
		}}, nil)
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
