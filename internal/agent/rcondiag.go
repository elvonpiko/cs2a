package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cs2a/internal/cs2"
	"cs2a/internal/rcon"
)

// RCONDiagnosis explains why RCON is unreachable and what would fix it.
//
// CS2 only opens its RCON listener when -usercon is on the launch line *and*
// rcon_password is set at the time the map loads. A server started before the
// password was written keeps the listener closed, which looks identical to a
// wrong address: "connect: connection refused". Reporting the cause is what
// turns that dead end into a fixable problem.
type RCONDiagnosis struct {
	OK bool `json:"ok"`
	// Addr is the address the agent dials.
	Addr string `json:"addr"`
	// Reason is the operator-facing explanation ("" when OK).
	Reason string `json:"reason,omitempty"`
	// Fix names the repair that would resolve it, when the agent can do it.
	Fix string `json:"fix,omitempty"`
	// Repairable reports whether RepairRCON can apply that fix.
	Repairable bool `json:"repairable"`
	// MissingUserCon / MissingPassword / AddrMismatch are the individual
	// findings, kept separate so the panel can word them itself.
	MissingUserCon  bool   `json:"missing_usercon"`
	MissingPassword bool   `json:"missing_password"`
	SuggestedAddr   string `json:"suggested_addr,omitempty"`
	// PasswordMismatch reports that the port is open and RCON is enabled, but
	// the agent's password is not the one the running server booted with. This
	// is a different problem from an unreachable port and needs a different
	// fix, yet both used to surface as "give it a minute to finish loading".
	PasswordMismatch bool `json:"password_mismatch,omitempty"`
}

// launchArgsReader is the optional part of a ServiceController that can report
// the unit's launch line. Tests supply a fake without it.
type launchArgsReader interface {
	UnitLaunchArgs() LaunchArgs
}

func (s *Server) launchArgs() LaunchArgs {
	if r, ok := s.sysd.(launchArgsReader); ok {
		return r.UnitLaunchArgs()
	}
	return LaunchArgs{}
}

// rconProbeTimeout bounds the diagnostic dial. It is short on purpose: the
// probe runs while an operator waits for a page, and a refused connection
// answers instantly while an unroutable address must not stall the request.
var rconProbeTimeout = 3 * time.Second

// DiagnoseRCON tries an RCON connection and, on failure, works out why.
func (s *Server) DiagnoseRCON() RCONDiagnosis {
	d := RCONDiagnosis{Addr: s.cfg.RCONAddr}
	c, err := rcon.Dial(s.cfg.RCONAddr, s.cfg.RCONPassword, rconProbeTimeout)
	if err == nil {
		c.Close()
		d.OK = true
		return d
	}
	dialErr := err

	// A rejected password means the listener is open and -usercon is on: the
	// only thing wrong is the secret. Diagnosing further would report fictional
	// problems (the launch line and server.cfg are both fine), and the previous
	// code fell through to "give the server a minute to load the map".
	if errors.Is(err, rcon.ErrAuthFailed) {
		d.PasswordMismatch = true
		d.Reason = "the game server refused the agent's rcon_password — the running server booted with a different one"
		d.Fix = "write the agent's rcon_password into server.cfg and restart the game server"
		// Repairable only if the agent actually has a password to install.
		d.Repairable = strings.TrimSpace(s.cfg.RCONPassword) != ""
		if !d.Repairable {
			d.Fix = "set an rcon_password for the agent, then repair again"
		}
		return d
	}

	args := s.launchArgs()
	if args.Found && !args.UserCon {
		d.MissingUserCon = true
	}
	if strings.TrimSpace(s.cfg.RCONPassword) == "" {
		d.MissingPassword = true
	} else if !s.serverCFGHasRCONPassword() {
		// The agent knows a password the game was never told about: the
		// listener stays closed until server.cfg carries it at boot.
		d.MissingPassword = true
	}
	if args.Found {
		if want := addrFromLaunch(args, s.cfg.RCONAddr); want != "" && want != s.cfg.RCONAddr {
			d.SuggestedAddr = want
		}
	}

	switch {
	case d.SuggestedAddr != "":
		d.Reason = fmt.Sprintf("the game server listens on %s, but the agent is configured to dial %s",
			d.SuggestedAddr, s.cfg.RCONAddr)
		d.Fix = "point the agent at the address the launch line uses"
		d.Repairable = true
	case d.MissingUserCon && d.MissingPassword:
		d.Reason = "the game server was started without -usercon and without an rcon_password, so it never opened its RCON port"
		d.Fix = "add -usercon to the launch line, write rcon_password into server.cfg, and restart the server"
		d.Repairable = true
	case d.MissingUserCon:
		d.Reason = "the game server's launch line has no -usercon, so RCON is disabled in the engine"
		d.Fix = "add -usercon to the launch line and restart the server"
		d.Repairable = true
	case d.MissingPassword:
		d.Reason = "the game server has no rcon_password set, so it does not open its RCON port"
		d.Fix = "write rcon_password into server.cfg and restart the server"
		d.Repairable = true
	default:
		// Everything looks configured. Distinguish "the port is closed" from
		// anything else: a refused connection while the unit is active is the
		// engine still loading its first map, whereas a timeout usually means a
		// firewall or a wrong address.
		d.Reason = "connecting failed: " + dialErr.Error()
		switch {
		case isConnRefused(dialErr):
			d.Fix = "if the server just started, give it a minute to finish loading the map"
		case isTimeout(dialErr):
			d.Fix = "check that nothing between the panel and " + s.cfg.RCONAddr + " is blocking that port"
		default:
			d.Fix = "check the game server's own log for why it did not accept the connection"
		}
	}
	return d
}

// isConnRefused reports a TCP-level refusal: nothing is listening on the port.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// isTimeout reports a dial or read that expired without an answer.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// serverCFGHasRCONPassword reports whether server.cfg sets a non-empty
// rcon_password, which is what the game reads at boot.
func (s *Server) serverCFGHasRCONPassword() bool {
	raw, err := os.ReadFile(filepath.Join(s.cfg.CFGDir(), "server.cfg"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		rest, ok := strings.CutPrefix(line, "rcon_password")
		if !ok {
			continue
		}
		val := strings.Trim(strings.TrimSpace(rest), `"`)
		if val != "" {
			return true
		}
	}
	return false
}

// addrFromLaunch returns the address the launch line implies, preserving the
// configured value when the launch line says nothing new.
func addrFromLaunch(args LaunchArgs, current string) string {
	if !args.Found {
		return ""
	}
	host := args.RCONHost()
	port := args.Port
	if port == 0 {
		// Keep the configured port when the launch line relies on the default.
		if _, p, ok := splitHostPort(current); ok {
			port = p
		} else {
			port = 27015
		}
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func splitHostPort(addr string) (host string, port int, ok bool) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", 0, false
	}
	host = addr[:i]
	var p int
	if _, err := fmt.Sscanf(addr[i+1:], "%d", &p); err != nil || p <= 0 {
		return "", 0, false
	}
	return host, p, true
}

// RepairRCON applies the fixes DiagnoseRCON identified and restarts the game
// server so the engine picks them up. It reports what it changed.
//
// This is an explicit operator action, not something Status does implicitly:
// it edits a systemd drop-in and restarts the game.
func (s *Server) RepairRCON(ctx context.Context) (applied []string, result ActionResult, err error) {
	d := s.DiagnoseRCON()
	if d.OK {
		return nil, ActionResult{Action: "repair", Active: true,
			Message: "RCON already works — nothing to repair."}, nil
	}

	if d.SuggestedAddr != "" {
		// Address corrections are applied in memory and persisted by the
		// caller; a restart is not needed for the agent to use the new value.
		if err := s.setRCONAddr(d.SuggestedAddr); err != nil {
			return nil, ActionResult{}, err
		}
		applied = append(applied, "pointed the agent at "+d.SuggestedAddr)
		if fixed := s.DiagnoseRCON(); fixed.OK {
			return applied, ActionResult{Action: "repair", Active: true,
				Message: "RCON reachable at " + d.SuggestedAddr + "."}, nil
		}
		d = s.DiagnoseRCON()
	}

	if d.MissingPassword {
		if strings.TrimSpace(s.cfg.RCONPassword) == "" {
			return applied, ActionResult{}, fmt.Errorf("no rcon_password is configured for the agent — set one in the agent config first")
		}
		if err := s.writeRCONPasswordToCFG(); err != nil {
			return applied, ActionResult{}, err
		}
		applied = append(applied, "wrote rcon_password into server.cfg")
	}

	if d.PasswordMismatch {
		// The port is open and the engine accepts connections; only the secret
		// disagrees. Writing the agent's password into server.cfg and restarting
		// makes the two match.
		if strings.TrimSpace(s.cfg.RCONPassword) == "" {
			return applied, ActionResult{}, fmt.Errorf("no rcon_password is configured for the agent — set one in the agent config first")
		}
		if err := s.writeRCONPasswordToCFG(); err != nil {
			return applied, ActionResult{}, err
		}
		applied = append(applied, "wrote the agent's rcon_password into server.cfg")
	}

	if d.MissingUserCon {
		changed, err := s.ensureUserCon()
		if err != nil {
			return applied, ActionResult{}, err
		}
		if changed {
			applied = append(applied, "added -usercon to the launch line")
		}
	}

	if len(applied) == 0 {
		return nil, ActionResult{Action: "repair",
			Message: "Nothing to repair automatically: " + d.Reason}, nil
	}

	// Both fixes only take effect at boot.
	res := s.Control(ctx, ActionRestart)
	return applied, res, nil
}

// setRCONAddr updates the addresses the agent dials and persists them when the
// agent knows where its config lives.
func (s *Server) setRCONAddr(addr string) error {
	oldA2S := s.cfg.A2SAddr
	s.cfg.RCONAddr = addr
	// A2S runs on the same socket pair; keep them consistent unless the
	// operator pointed them somewhere different on purpose.
	if oldA2S == "" || oldA2S == "127.0.0.1:27015" || sameHostPort(oldA2S, addr) {
		s.cfg.A2SAddr = addr
	}
	return s.cfg.Persist()
}

func sameHostPort(a, b string) bool {
	ha, pa, oka := splitHostPort(a)
	hb, pb, okb := splitHostPort(b)
	if !oka || !okb {
		return false
	}
	return ha == hb || pa == pb
}

// writeRCONPasswordToCFG adds rcon_password to the managed block of server.cfg.
// The managed block is used rather than a raw append so a repeat repair updates
// the value instead of stacking duplicate lines.
func (s *Server) writeRCONPasswordToCFG() error {
	return s.ApplyManagedSettings(context.Background(), append(
		s.currentSettingsSans("rcon_password"),
		managedRCONPassword(s.cfg.RCONPassword)))
}

// managedRCONPassword is the managed-block entry for the agent's password.
func managedRCONPassword(pw string) cs2.CFGSetting {
	return cs2.CFGSetting{
		Name:    "rcon_password",
		Value:   pw,
		Comment: "required by cs2a: the engine only opens its RCON port when this is set at boot",
	}
}

// userConDropIn is the systemd drop-in that adds -usercon to an adopted unit.
// A drop-in is used rather than editing the unit: the operator's file stays
// exactly as they wrote it, and removing the drop-in fully reverts the change.
const userConDropIn = "10-cs2a-usercon.conf"

// ensureUserCon appends -usercon to the game unit's launch line through a
// systemd drop-in. It reports whether anything changed.
func (s *Server) ensureUserCon() (bool, error) {
	args := s.launchArgs()
	if !args.Found {
		return false, fmt.Errorf("could not read the game server's launch line from systemd")
	}
	if args.UserCon {
		return false, nil
	}
	dropIn, ok := s.sysd.(dropInWriter)
	if !ok {
		return false, fmt.Errorf("this system cannot be repaired automatically — add -usercon to the launch line by hand")
	}
	// ExecStart must be cleared before being redefined, or systemd appends a
	// second command instead of replacing the first.
	content := fmt.Sprintf(`# Written by cs2a: CS2 only opens its RCON port when -usercon is present.
# Delete this file to revert.
[Service]
ExecStart=
ExecStart=%s -usercon
`, args.Raw)
	if err := dropIn.WriteDropIn(userConDropIn, content); err != nil {
		return false, err
	}
	return true, nil
}

// dropInWriter is implemented by controllers that can install a systemd
// drop-in for the game unit.
type dropInWriter interface {
	WriteDropIn(name, content string) error
}
