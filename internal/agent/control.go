package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Action is a service lifecycle operation the panel can request.
type Action string

// The three lifecycle actions.
const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
)

// ActionResult reports what actually happened to the unit.
//
// The old behaviour was to return {"ok":true} as soon as systemctl exited,
// which is why "starting and restarting buttons do not show any errors but I
// don't know if they actually worked": systemctl exits 0 for `start` the moment
// the process is forked, and a CS2 server that dies three seconds later during
// map load looks identical to a healthy one. Control therefore waits for the
// unit to settle and, when it fails, brings the journal back with it.
type ActionResult struct {
	Action string `json:"action"`
	// Active is the unit state after the action settled.
	Active bool `json:"active"`
	// Sub is systemd's SubState ("running", "failed", "auto-restart", …).
	Sub string `json:"sub,omitempty"`
	// Message is operator-facing text describing the outcome.
	Message string `json:"message"`
	// Failed marks an action that did not reach the requested state.
	Failed bool `json:"failed"`
	// Log carries the tail of the unit's journal when something went wrong.
	Log []string `json:"log,omitempty"`
}

// stateReader is the optional richer state surface of a ServiceController.
type stateReader interface {
	ActiveState() (active, sub, result string)
}

// journalReader is implemented by controllers that can read the unit's log.
type journalReader interface {
	JournalTail(n int) ([]string, error)
}

// settleWindow is how long Control waits for a unit to reach a stable state.
// A CS2 server that fails at startup (bad launch args, missing GSLT, port in
// use) usually dies within a few seconds; a healthy one is "active/running"
// almost immediately even though map load takes longer.
var settleWindow = 6 * time.Second

// settlePoll is the interval between state checks while settling.
var settlePoll = 300 * time.Millisecond

// Control performs a lifecycle action and verifies the result.
func (s *Server) Control(ctx context.Context, action Action) ActionResult {
	res := ActionResult{Action: string(action)}

	var err error
	switch action {
	case ActionStart:
		err = s.sysd.Start()
	case ActionStop:
		err = s.sysd.Stop()
	case ActionRestart:
		err = s.sysd.Restart()
	default:
		res.Failed = true
		res.Message = fmt.Sprintf("unknown action %q", action)
		return res
	}
	if err != nil {
		res.Failed = true
		res.Message = "systemd refused the " + string(action) + ": " + trimSystemdErr(err.Error())
		res.Log = s.journalTail()
		return res
	}

	wantActive := action != ActionStop
	active, sub := s.awaitState(ctx, wantActive)
	res.Active = active
	res.Sub = sub

	switch {
	case wantActive && active && sub != "failed":
		res.Message = "Server is running. It needs up to a minute to finish loading the map before players can connect."
	case wantActive:
		res.Failed = true
		res.Message = "The server did not stay running — it exited right after " +
			map[Action]string{ActionStart: "starting", ActionRestart: "restarting"}[action] + "."
		res.Log = s.journalTail()
	case active:
		res.Failed = true
		res.Message = "The server is still running after the stop request."
		res.Log = s.journalTail()
	default:
		res.Message = "Server stopped."
	}
	return res
}

// awaitState polls the unit until the requested state is reached and holds.
//
// Stopping is confirmed by the first observation of an inactive unit. Starting
// deliberately watches for the whole window instead of returning on the first
// "active": systemd marks a forked process active immediately, and a CS2 server
// with bad launch arguments or a busy port dies a second or two later. Waiting
// is what makes the button's answer trustworthy.
func (s *Server) awaitState(ctx context.Context, want bool) (active bool, sub string) {
	deadline := time.Now().Add(settleWindow)
	for {
		active, sub = s.readState()
		switch {
		case !want && !active:
			return active, sub // stop confirmed
		case want && (!active || sub == "failed" || sub == "auto-restart"):
			return active, sub // start failed; no point waiting longer
		}
		if !time.Now().Before(deadline) {
			return active, sub
		}
		select {
		case <-ctx.Done():
			return active, sub
		case <-time.After(settlePoll):
		}
	}
}

func (s *Server) readState() (bool, string) {
	if r, ok := s.sysd.(stateReader); ok {
		activeState, sub, _ := r.ActiveState()
		if activeState != "" {
			return activeState == "active" || activeState == "reloading", sub
		}
	}
	active, _ := s.sysd.IsActive()
	return active, ""
}

// journalTail returns the last lines of the unit log, or nil when unavailable.
func (s *Server) journalTail() []string {
	r, ok := s.sysd.(journalReader)
	if !ok {
		return nil
	}
	lines, err := r.JournalTail(40)
	if err != nil {
		return nil
	}
	return filterJournalNoise(lines)
}

// Logs returns the tail of the game server's journal for the UI.
func (s *Server) Logs(n int) ([]string, error) {
	r, ok := s.sysd.(journalReader)
	if !ok {
		return nil, fmt.Errorf("logs: journal is not available on this system")
	}
	lines, err := r.JournalTail(n)
	if err != nil {
		return nil, err
	}
	return filterJournalNoise(lines), nil
}

// filterJournalNoise drops blank lines and systemd's own "-- No entries --"
// placeholder so an empty log renders as empty rather than as one odd line.
func filterJournalNoise(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "-- No entries") {
			continue
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// trimSystemdErr shortens systemctl's stderr to its useful sentence.
func trimSystemdErr(msg string) string {
	msg = strings.TrimSpace(msg)
	// "systemctl start cs2-server: exit status 1: Job for cs2-server.service
	// failed because … See "systemctl status …" for details."
	if i := strings.Index(msg, "See \""); i > 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	return strings.TrimSuffix(msg, ".")
}
