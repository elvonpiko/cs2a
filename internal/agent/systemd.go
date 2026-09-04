package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Systemd controls the game server's systemd unit. All exec paths come from
// config/flags, never from API input; user-supplied values never reach a
// command line (service name is validated against a safe charset).
type Systemd struct {
	bin         string
	serviceName string
}

// NewSystemd builds a controller for the given unit name.
func NewSystemd(serviceName string) *Systemd {
	return &Systemd{bin: "systemctl", serviceName: sanitizeUnit(serviceName)}
}

func sanitizeUnit(name string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '@':
			return r
		default:
			return -1
		}
	}, name)
	if out == "" || out == "." || out == ".." {
		return "cs2-server"
	}
	return out
}

func (s *Systemd) run(timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Start starts the unit (no-op if already running).
func (s *Systemd) Start() error { return s.run(30*time.Second, "start", s.serviceName) }

// Stop stops the unit.
func (s *Systemd) Stop() error { return s.run(60*time.Second, "stop", s.serviceName) }

// Restart restarts the unit.
func (s *Systemd) Restart() error { return s.run(90*time.Second, "restart", s.serviceName) }

// IsActive reports whether the unit is currently running.
func (s *Systemd) IsActive() (bool, error) {
	cmd := exec.Command(s.bin, "is-active", s.serviceName)
	out, err := cmd.Output()
	if err != nil {
		// is-active exits non-zero for inactive/failed units; that's an
		// answer, not an error.
		return strings.TrimSpace(string(out)) == "active", nil
	}
	return strings.TrimSpace(string(out)) == "active", nil
}

// IsEnabled reports whether the unit is enabled at boot.
func (s *Systemd) IsEnabled() (bool, error) {
	cmd := exec.Command(s.bin, "is-enabled", s.serviceName)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out)) == "enabled", nil
}

// JournalTail returns the last n lines of the unit's journal.
func (s *Systemd) JournalTail(n int) ([]string, error) {
	if n <= 0 || n > 500 {
		n = 100
	}
	cmd := exec.Command("journalctl", "-u", s.serviceName, "-n", fmt.Sprint(n), "--no-pager", "-q")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("journalctl: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	return lines, nil
}
