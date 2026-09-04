// Package bootstrap provides testable, OS-aware helpers used by the
// interactive bootstrap installer (scripts/bootstrap.sh). Everything the
// script does is expressed here as small functions so they can be unit
// tested without a real VPS.
package bootstrap

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Plan describes everything the installer will do, gathered before any
// mutation so the user can review it.
type Plan struct {
	InstallRoot string // e.g. /opt/cs2a
	CS2Dir      string // e.g. /opt/cs2a/cs2 (contains game/)
	AgentUser   string // e.g. steam
	ListenPanel string // 0.0.0.0:8080
	ListenAgent string // 127.0.0.1:8100
	GamePort    int    // 27015
	Domain      string // optional reverse-proxy hostname (informational)
	WithCS2     bool   // install SteamCMD + CS2 server too
}

// Secrets carries everything generated once and shown at the end.
type Secrets struct {
	AgentToken string
	PanelToken string // agent_token as seen by panel (same value)
	SetupToken string // first-admin signup token
	RCONPass   string
	AdminUser  string
	AdminPass  string
}

// DefaultPlan returns the standard layout for a root install.
func DefaultPlan() Plan {
	return Plan{
		InstallRoot: "/opt/cs2a",
		CS2Dir:      "/opt/cs2a/cs2",
		AgentUser:   "steam",
		ListenPanel: "0.0.0.0:8080",
		ListenAgent: "127.0.0.1:8100",
		GamePort:    27015,
		WithCS2:     true,
	}
}

// GenerateToken returns a URL-safe random token of ~32 bytes entropy.
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// GeneratePassword returns a readable random password (no ambiguous chars).
func GeneratePassword() (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(out), nil
}

// CheckSystem validates OS/arch constraints and returns problems.
func CheckSystem() []string {
	var problems []string
	if runtime.GOOS != "linux" {
		problems = append(problems, fmt.Sprintf("unsupported OS %q (linux required for the systemd-based install)", runtime.GOOS))
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		problems = append(problems, fmt.Sprintf("unsupported arch %q", runtime.GOARCH))
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		problems = append(problems, "systemd not found (systemctl missing)")
	}
	return problems
}

// MissingTools lists required commands not present on PATH.
func MissingTools() []string {
	required := []string{"curl", "tar", "gzip"}
	var missing []string
	for _, t := range required {
		if _, err := exec.LookPath(t); err != nil {
			missing = append(missing, t)
		}
	}
	return missing
}

// DetectPublicIP finds the primary outbound IPv4 (informational display).
func DetectPublicIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}

// ValidateCS2Dir reports whether dir looks like a CS2 install root.
func ValidateCS2Dir(dir string) error {
	gi := filepath.Join(dir, "game", "csgo", "gameinfo.gi")
	if _, err := os.Stat(gi); err != nil {
		return fmt.Errorf("%s does not look like a CS2 install (missing %s)", dir, gi)
	}
	return nil
}

// SystemdUnit renders the cs2-server unit for the game.
func SystemdUnit(p Plan, rconPassword, gslt string) string {
	execStart := fmt.Sprintf("%s/game/cs2.sh -dedicated -console -usercon +ip 0.0.0.0 +port %d +map de_dust2 +exec server.cfg",
		p.CS2Dir, p.GamePort)
	if gslt != "" {
		execStart += " +sv_setsteamaccount " + gslt
	}
	return fmt.Sprintf(`[Unit]
Description=CS2 dedicated server (managed by cs2a)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
Group=%s
WorkingDirectory=%s/game
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, p.AgentUser, p.AgentUser, p.CS2Dir, execStart)
}

// AgentUnit renders the cs2a-agent service unit.
func AgentUnit(p Plan) string {
	return fmt.Sprintf(`[Unit]
Description=cs2a agent (CS2 server manager)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=%s/bin/cs2a-agent -config %s/etc/agent.json
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
`, p.InstallRoot, p.InstallRoot)
}

// PanelUnit renders the cs2a-panel service unit. Admin credentials come from
// a 0600 EnvironmentFile written by the installer.
func PanelUnit(p Plan) string {
	return fmt.Sprintf(`[Unit]
Description=cs2a panel (control plane UI)
After=network-online.target cs2a-agent.service
Wants=network-online.target

[Service]
Type=simple
User=root
EnvironmentFile=%s/etc/panel.env
ExecStart=%s/bin/cs2a-panel -config %s/etc/panel.json
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
`, p.InstallRoot, p.InstallRoot, p.InstallRoot)
}

// PanelEnv renders the panel EnvironmentFile content (0600, contains secret).
func PanelEnv(adminUser, adminPass string) string {
	return "CS2A_ADMIN_USER=" + adminUser + "\nCS2A_ADMIN_PASSWORD=" + adminPass + "\n"
}

// FirewallCommands returns the iptables/ufw commands for the chosen ports.
func FirewallCommands(p Plan) []string {
	return []string{
		fmt.Sprintf("ufw allow %d/tcp", p.GamePort), // RCON (localhost used, but remote admins may need it)
		fmt.Sprintf("ufw allow %d/udp", p.GamePort), // game + A2S
	}
}

// SteamCMDCmds returns the steamcmd invocation to install/update CS2.
func SteamCMDCmds(p Plan) []string {
	return []string{
		fmt.Sprintf("steamcmd +force_install_dir %s +login anonymous +app_update 730 validate +quit", p.CS2Dir),
	}
}

// SplitHostPort is a tiny helper for display code.
func SplitHostPort(addr string) (string, string) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return addr, ""
	}
	return addr[:i], addr[i+1:]
}
