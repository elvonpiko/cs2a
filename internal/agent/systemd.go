package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// UptimeSeconds reports seconds since the unit last became active.
func (s *Systemd) UptimeSeconds() (float64, bool) {
	// --timestamp=unix (systemd 247+) gives an unambiguous "@<epoch>" value.
	// Older systemd ignores the flag and prints its locale-ish default
	// ("Sat 2026-09-05 04:40:19 +0330"), which is why the human formats are
	// still parsed below.
	cmd := exec.Command(s.bin, "show", s.serviceName, "--timestamp=unix", "--property=ActiveEnterTimestamp", "--value")
	out, err := cmd.Output()
	if err != nil {
		cmd = exec.Command(s.bin, "show", s.serviceName, "--property=ActiveEnterTimestamp", "--value")
		out, err = cmd.Output()
		if err != nil {
			return 0, false
		}
	}
	t, ok := parseSystemdTimestamp(strings.TrimSpace(string(out)))
	if !ok {
		return 0, false
	}
	secs := time.Since(t).Seconds()
	if secs < 0 {
		return 0, false
	}
	return secs, true
}

// systemdTimeLayouts covers every ActiveEnterTimestamp shape systemd emits:
// its default weekday-prefixed form (with a numeric offset or a zone
// abbreviation), the same without the weekday, and ISO-8601.
var systemdTimeLayouts = []string{
	"Mon 2006-01-02 15:04:05 -0700",
	"Mon 2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05 MST",
	"2006-01-02T15:04:05-0700",
	time.RFC3339,
}

// parseSystemdTimestamp parses a systemctl show timestamp value. An empty or
// zero value ("n/a", "0") means the unit never became active.
func parseSystemdTimestamp(ts string) (time.Time, bool) {
	if ts == "" || ts == "n/a" || ts == "0" || ts == "@0" {
		return time.Time{}, false
	}
	if epoch, ok := strings.CutPrefix(ts, "@"); ok {
		// unix form: "@1788570619" (may carry fractional seconds)
		if dot := strings.IndexByte(epoch, '.'); dot >= 0 {
			epoch = epoch[:dot]
		}
		secs, err := strconv.ParseInt(epoch, 10, 64)
		if err != nil || secs <= 0 {
			return time.Time{}, false
		}
		return time.Unix(secs, 0), true
	}
	for _, layout := range systemdTimeLayouts {
		if t, err := time.Parse(layout, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
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

// dropInDir is where systemd reads per-unit overrides from.
func (s *Systemd) dropInDir() string {
	return filepath.Join("/etc/systemd/system", s.serviceName+".service.d")
}

// WriteDropIn installs a systemd drop-in for the game unit and reloads the
// daemon so it takes effect on the next start. The unit file the operator wrote
// is never modified: deleting the drop-in restores the original launch line
// exactly.
func (s *Systemd) WriteDropIn(name, content string) error {
	if name == "" || strings.ContainsAny(name, "/\\") || !strings.HasSuffix(name, ".conf") {
		return fmt.Errorf("systemd: invalid drop-in name %q", name)
	}
	dir := s.dropInDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("systemd: create %s: %w", dir, err)
	}
	if err := atomicWrite(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		return fmt.Errorf("systemd: write drop-in: %w", err)
	}
	return s.DaemonReload()
}

// DaemonReload makes systemd re-read unit files.
func (s *Systemd) DaemonReload() error {
	return s.run(30*time.Second, "daemon-reload")
}
