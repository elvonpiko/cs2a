package agent

import (
	"context"
	"errors"
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
	// Diag explains an unreachable RCON and what would fix it. Present only
	// when the server is running but RCON did not answer.
	Diag *RCONDiagnosis `json:"diag,omitempty"`
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
	if st, err := s.queryRCONStatus(ctx); err != nil {
		// A raw "dial tcp 127.0.0.1:27015: connect: connection refused" is
		// what the panel used to show. Work out the actual cause instead, so
		// the UI can offer the fix rather than the symptom.
		diag := s.DiagnoseRCON()
		if !diag.OK && diag.Reason != "" {
			out.Diag = &diag
			out.Note = joinNotes(out.Note, "rcon: "+diag.Reason)
		} else {
			out.Note = joinNotes(out.Note, "rcon: "+err.Error())
		}
		// A partial answer is still worth publishing: the note explains it.
		if len(st.Players) > 0 || st.Hostname != "" || st.Map != "" {
			out.Rcon = &st
		}
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
	// The socket timeout is derived from the caller's budget: a fixed 3 s
	// timeout with a 4 s context let one slow read run past the deadline, and
	// the status request could overrun the panel's own 15 s limit once the RCON
	// leg was added.
	cctx, cancel := context.WithTimeout(ctx, a2sQueryBudget)
	defer cancel()
	var info a2s.Info
	c, err := a2s.Dial(s.cfg.A2SAddr, a2sQueryBudget)
	if err != nil {
		return info, err
	}
	defer c.Close()
	return c.Info(cctx)
}

// a2sQueryBudget bounds the whole A2S leg of a status query, challenge round
// trips included.
const a2sQueryBudget = 3 * time.Second

func (s *Server) queryRCONStatus(ctx context.Context) (cs2.Status, error) {
	var st cs2.Status
	c, err := rcon.Dial(s.cfg.RCONAddr, s.cfg.RCONPassword, 5*time.Second)
	if err != nil {
		return st, err
	}
	defer c.Close()
	// A hibernating or map-loading server can hold a `status` for the client's
	// full 20 s budget. Bounding it by the caller's context means a cancelled
	// page request stops the wait instead of pinning the agent.
	cctx, cancel := context.WithTimeout(ctx, rconStatusBudget)
	defer cancel()
	out, err := c.ExecContext(cctx, "status")
	if err != nil {
		// A truncated answer still carries the fields that did arrive; showing
		// them beats an empty page, and the note reports the truncation.
		if out == "" || !errors.Is(err, rcon.ErrTruncated) {
			return st, err
		}
		return cs2.ParseStatus(out), err
	}
	return cs2.ParseStatus(out), nil
}

// rconStatusBudget bounds the RCON leg of a status query.
const rconStatusBudget = 6 * time.Second

// Lifecycle actions live in control.go: they must verify the unit actually
// reached the requested state, which a bare systemctl call cannot do.

// Exec runs a raw server console command via RCON (admin-only surface).
//
// A truncated answer is returned together with its error: for the console the
// partial text is the useful part, and the caller labels it as incomplete rather
// than presenting half a `cvarlist` as the whole thing.
func (s *Server) Exec(ctx context.Context, command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" || len(command) > 500 {
		return "", fmt.Errorf("rcon: invalid command")
	}
	c, err := rcon.Dial(s.cfg.RCONAddr, s.cfg.RCONPassword, 5*time.Second)
	if err != nil {
		return "", s.rconDialError(err)
	}
	defer c.Close()
	return c.ExecContext(ctx, command)
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
	return s.rconFire(fmt.Sprintf("changelevel %s", mapName))
}

// reWorkshopID matches a bare workshop file id, which needs
// host_workshop_map instead of changelevel.
var reWorkshopID = regexp.MustCompile(`^[0-9]{6,20}$`)

// ChangeMapForce skips local validation (workshop maps and collection ids).
func (s *Server) ChangeMapForce(mapName string) error {
	if !reMapName.MatchString(mapName) {
		return fmt.Errorf("map: invalid map name %q", mapName)
	}
	if reWorkshopID.MatchString(mapName) {
		return s.rconFire(fmt.Sprintf("host_workshop_map %s", mapName))
	}
	return s.rconFire(fmt.Sprintf("changelevel %s", mapName))
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

// ManagedBlockWarning reports a server.cfg that cs2a can write to but cannot
// fully control — today that means a duplicate managed block, whose second copy
// overrides everything the panel writes into the first. Returning it as a
// warning keeps the save working while telling the operator why the server may
// not match the UI.
func (s *Server) ManagedBlockWarning() string {
	content, err := cs2.LoadServerCFG(s.cfg.CFGDir())
	if err != nil {
		return ""
	}
	return cs2.ManagedBlockConflict(content)
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
	s.pushSettingsLive(ctx, settings)
	return nil
}

// pushSettingsLive applies the saved cvars to the running server so the operator
// does not have to restart. Reaching RCON is best-effort — the file is the
// source of truth — but the work is done over ONE connection: dialling and
// authenticating per cvar meant a 12-setting save opened 12 sockets, and each
// `rcon_password` change in the batch dropped the connections still in flight.
func (s *Server) pushSettingsLive(ctx context.Context, settings []cs2.CFGSetting) {
	live := make([]cs2.CFGSetting, 0, len(settings))
	for _, set := range settings {
		// A bare `mp_autokick` line is a query, not an assignment; pushing it
		// as `mp_autokick ""` would set the cvar to 0.
		if !set.Bare {
			live = append(live, set)
		}
	}
	if len(live) == 0 {
		return
	}
	c, err := rcon.Dial(s.cfg.RCONAddr, s.cfg.RCONPassword, 5*time.Second)
	if err != nil {
		return
	}
	defer c.Close()
	cctx, cancel := context.WithTimeout(ctx, time.Duration(len(live))*time.Second+5*time.Second)
	defer cancel()
	for _, set := range live {
		if cctx.Err() != nil {
			return
		}
		if _, err := c.ExecContext(cctx, cs2.CvarCommand(set.Name, set.Value)); err != nil {
			return // a dead connection will not revive for the next cvar
		}
	}
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

// ExecQuiet runs a command and ignores the result. Used for best-effort
// plugin nudges (e.g. wl_reload) where "the plugin is not installed" and
// "RCON is down" are both acceptable outcomes.
func (s *Server) ExecQuiet(command string) error {
	return s.rconFire(command)
}

// rconFire sends a command that makes the server unresponsive while it runs
// (changelevel, host_workshop_map). The server frequently never answers those,
// so a missing reply is treated as success — the panel must not surface a
// bogus "timeout" for an operation that actually started.
func (s *Server) rconFire(command string) error {
	c, err := rcon.Dial(s.cfg.RCONAddr, s.cfg.RCONPassword, 5*time.Second)
	if err != nil {
		return s.rconDialError(err)
	}
	defer c.Close()
	_, err = c.Fire(command, 2*time.Second)
	return err
}

// rconDialError replaces a bare socket error with the diagnosed cause. The
// operator who saw "rcon: dial 127.0.0.1:27015: connect: connection refused"
// had no way to know the game server was bound elsewhere or started without
// -usercon; that is exactly what the diagnosis reports.
func (s *Server) rconDialError(err error) error {
	d := s.DiagnoseRCON()
	if d.OK || d.Reason == "" {
		return err
	}
	if d.Fix != "" {
		return fmt.Errorf("%s — %s", d.Reason, d.Fix)
	}
	return errors.New(d.Reason)
}
