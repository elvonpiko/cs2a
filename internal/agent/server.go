package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"cs2a/internal/a2s"
	"cs2a/internal/cs2"
	"cs2a/internal/rcon"
)

// ServiceController abstracts service lifecycle (systemd in production,
// fakes in tests).
type ServiceController interface {
	Start() error
	Stop() error
	Restart() error
	IsActive() (bool, error)
	IsEnabled() (bool, error)
	// UptimeSeconds reports how long the unit has been running; ok=false
	// when unknown.
	UptimeSeconds() (float64, bool)
}

// Server executes operations against the CS2 server: service control via
// systemd, queries via RCON/A2S, and config via the managed server.cfg block.
type Server struct {
	cfg   Config
	sysd  ServiceController
	store *Store
}

// NewServer builds the server service.
func NewServer(cfg Config, store *Store) *Server {
	return &Server{cfg: cfg, sysd: NewSystemd(cfg.ServiceName), store: store}
}

// ServiceStatus mirrors systemd state for the panel.
type ServiceStatus struct {
	Active        bool    `json:"active"`
	Sub           string  `json:"sub,omitempty"`
	Enabled       bool    `json:"enabled"`
	UptimeSeconds float64 `json:"uptime_seconds,omitempty"`
}

// FullStatus is the /api/v1/server/status payload.
type FullStatus struct {
	Service ServiceStatus `json:"service"`
	Info    *a2s.Info     `json:"info,omitempty"` // A2S_INFO (may be nil when stopped)
	Rcon    *cs2.Status   `json:"rcon,omitempty"` // RCON status (players, ids)
	Note    string        `json:"note,omitempty"` // non-fatal query problems
}

// Status composes systemd + A2S + RCON state. Individual query failures are
// reported in Note rather than failing the whole call.
func (s *Server) Status(ctx context.Context) FullStatus {
	var out FullStatus
	active, err := s.sysd.IsActive()
	if err != nil {
		out.Note = "systemd: " + err.Error()
	}
	out.Service.Active = active
	if enabled, err := s.sysd.IsEnabled(); err == nil {
		out.Service.Enabled = enabled
	}
	if secs, ok := s.sysd.UptimeSeconds(); ok {
		out.Service.UptimeSeconds = secs
	}
	if !active {
		return out
	}
	if info, err := s.queryA2S(ctx); err != nil {
		out.Note = joinNotes(out.Note, "a2s: "+err.Error())
	} else {
		out.Info = &info
	}
	if st, err := s.queryRCONStatus(); err != nil {
		out.Note = joinNotes(out.Note, "rcon: "+err.Error())
	} else {
		out.Rcon = &st
	}
	return out
}

func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func (s *Server) queryA2S(ctx context.Context) (a2s.Info, error) {
	var info a2s.Info
	c, err := a2s.Dial(s.cfg.A2SAddr, 3*time.Second)
	if err != nil {
		return info, err
	}
	defer c.Close()
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return c.Info(cctx)
}

func (s *Server) queryRCONStatus() (cs2.Status, error) {
	var st cs2.Status
	c, err := rcon.Dial(s.cfg.RCONAddr, s.cfg.RCONPassword, 5*time.Second)
	if err != nil {
		return st, err
	}
	defer c.Close()
	out, err := c.Exec("status")
	if err != nil {
		return st, err
	}
	return cs2.ParseStatus(out), nil
}

// Start starts the game server unit.
func (s *Server) Start() error { return s.sysd.Start() }

// Stop stops the game server unit.
func (s *Server) Stop() error { return s.sysd.Stop() }

// Restart restarts the game server unit.
func (s *Server) Restart() error { return s.sysd.Restart() }

// Exec runs a raw server console command via RCON (admin-only surface).
func (s *Server) Exec(ctx context.Context, command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" || len(command) > 500 {
		return "", fmt.Errorf("rcon: invalid command")
	}
	c, err := rcon.Dial(s.cfg.RCONAddr, s.cfg.RCONPassword, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.Exec(command)
}

var reMapName = regexp.MustCompile(`^[a-z0-9_\-]{2,64}$`)

// Maps lists playable maps found in the maps directory (workshop excluded).
func (s *Server) Maps() ([]string, error) {
	dir := filepath.Join(s.cfg.CSGODir(), "maps")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil // fresh install: no maps yet
		}
		return nil, err
	}
	seen := map[string]struct{}{}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".vpk") {
			continue
		}
		base := strings.TrimSuffix(name, ".vpk")
		// vpk archives ship as de_dust2.vpk plus _dir/_000 sidecars
		base = strings.TrimSuffix(base, "_dir")
		if !reMapName.MatchString(base) {
			continue
		}
		seen[base] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}

// ChangeMap validates the map name against the installed maps, then executes
// changelevel over RCON.
func (s *Server) ChangeMap(ctx context.Context, mapName string) error {
	if !reMapName.MatchString(mapName) {
		return fmt.Errorf("map: invalid map name %q", mapName)
	}
	maps, err := s.Maps()
	if err != nil {
		return err
	}
	found := false
	for _, m := range maps {
		if m == mapName {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("map: %q is not installed", mapName)
	}
	return s.rconExec(fmt.Sprintf("changelevel %s", mapName))
}

// ChangeMapForce skips local validation (workshop maps).
func (s *Server) ChangeMapForce(mapName string) error {
	if !reMapName.MatchString(mapName) {
		return fmt.Errorf("map: invalid map name %q", mapName)
	}
	return s.rconExec(fmt.Sprintf("changelevel %s", mapName))
}

// ManagedSettings returns the current cs2a-managed server.cfg settings.
func (s *Server) ManagedSettings() ([]cs2.CFGSetting, error) {
	content, err := cs2.LoadServerCFG(s.cfg.CFGDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cs2.ExtractManagedBlock(content), nil
}

// ApplyManagedSettings writes the managed block into server.cfg and applies
// each cvar live over RCON (best-effort).
func (s *Server) ApplyManagedSettings(ctx context.Context, settings []cs2.CFGSetting) error {
	content, err := cs2.LoadServerCFG(s.cfg.CFGDir())
	if os.IsNotExist(err) {
		content = ""
	} else if err != nil {
		return err
	}
	if err := cs2.SaveServerCFG(s.cfg.CFGDir(), cs2.ApplyManagedBlock(content, settings)); err != nil {
		return err
	}
	// apply live; failure to reach RCON is not fatal for persistence
	for _, set := range settings {
		_ = s.rconExec(fmt.Sprintf("%s %q", set.Name, set.Value))
	}
	return nil
}

// SetPassword is a convenience wrapper: sets (or clears) sv_password.
func (s *Server) SetPassword(ctx context.Context, password string) error {
	val := password
	if strings.TrimSpace(val) == "" {
		val = "0" // clearing
	}
	return s.ApplyManagedSettings(ctx, append(s.currentSettingsSans("sv_password"),
		cs2.CFGSetting{Name: "sv_password", Value: val, Comment: "managed by cs2a"}))
}

func (s *Server) currentSettingsSans(name string) []cs2.CFGSetting {
	var out []cs2.CFGSetting
	if cur, err := s.ManagedSettings(); err == nil {
		for _, set := range cur {
			if set.Name != name {
				out = append(out, set)
			}
		}
	}
	return out
}

func (s *Server) rconExec(command string) error {
	c, err := rcon.Dial(s.cfg.RCONAddr, s.cfg.RCONPassword, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Exec(command)
	return err
}
