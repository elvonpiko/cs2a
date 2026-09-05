package bootstrap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateToken(t *testing.T) {
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateToken()
	if len(a) != 43 || a == b {
		t.Fatalf("bad tokens: %q %q", a, b)
	}
}

func TestGeneratePassword(t *testing.T) {
	pw, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) != 16 {
		t.Fatalf("len = %d", len(pw))
	}
	// no ambiguous characters
	for _, c := range pw {
		if strings.ContainsRune("O0oIl1", c) {
			t.Fatalf("ambiguous char %q in %q", c, pw)
		}
	}
}

func TestSystemdUnitShape(t *testing.T) {
	p := DefaultPlan()
	unit := SystemdUnit(p, "rconpw", "GSLT123")
	for _, want := range []string{
		"User=steam",
		"ExecStart=/opt/cs2a/cs2/game/cs2.sh",
		"-usercon",
		"-ip 0.0.0.0",
		"-port 27015",
		"-maxplayers 12",
		"+sv_setsteamaccount GSLT123",
		"Restart=on-failure",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
	// without GSLT the flag must be omitted
	unit2 := SystemdUnit(p, "rconpw", "")
	if strings.Contains(unit2, "sv_setsteamaccount") {
		t.Error("gslt flag present without token")
	}
	// the RCON password must never reach the command line (it lives in
	// server.cfg, which is not world-readable)
	if strings.Contains(unit, "rconpw") {
		t.Errorf("rcon password leaked into the unit:\n%s", unit)
	}
}

func TestAgentAndPanelUnits(t *testing.T) {
	p := DefaultPlan()
	agent := AgentUnit(p)
	if !strings.Contains(agent, "ExecStart=/opt/cs2a/bin/cs2a-agent -config /opt/cs2a/etc/agent.json") {
		t.Errorf("agent unit broken:\n%s", agent)
	}
	panel := PanelUnit(p)
	if !strings.Contains(panel, "EnvironmentFile=/opt/cs2a/etc/panel.env") {
		t.Errorf("panel unit broken:\n%s", panel)
	}
	env := PanelEnv("admin", "s3cret")
	if !strings.Contains(env, "CS2A_ADMIN_USER=admin") || !strings.Contains(env, "CS2A_ADMIN_PASSWORD=s3cret") {
		t.Errorf("panel env broken: %q", env)
	}
}

func TestValidateCS2Dir(t *testing.T) {
	dir := t.TempDir()
	if err := ValidateCS2Dir(dir); err == nil {
		t.Fatal("empty dir should not validate")
	}
	if err := mkGameinfo(dir); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCS2Dir(dir); err != nil {
		t.Fatalf("valid dir rejected: %v", err)
	}
}

func mkGameinfo(dir string) error {
	p := dir + "/game/csgo/gameinfo.gi"
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte("Game csgo\n"), 0o644)
}

func TestFirewallAndSteamCMDCmds(t *testing.T) {
	p := DefaultPlan()
	fw := FirewallCommands(p)
	// the panel port must be opened too, or the user cannot reach the UI
	want := []string{"27015/udp", "27015/tcp", "8080/tcp"}
	if len(fw) != len(want) {
		t.Fatalf("fw = %v", fw)
	}
	for i, w := range want {
		if !strings.Contains(fw[i], w) {
			t.Fatalf("fw[%d] = %q, want %q", i, fw[i], w)
		}
	}
	// with a domain Caddy answers on 80/443 and the raw port stays closed
	p.Domain = "panel.example.com"
	fw = FirewallCommands(p)
	joined := strings.Join(fw, " ")
	if !strings.Contains(joined, "80/tcp") || !strings.Contains(joined, "443/tcp") {
		t.Fatalf("fw with domain = %v", fw)
	}
	if strings.Contains(joined, "8080/tcp") {
		t.Fatalf("raw panel port opened behind caddy: %v", fw)
	}

	sc := SteamCMDCmds(DefaultPlan())
	if !strings.Contains(sc[0], "+force_install_dir /opt/cs2a/cs2") || !strings.Contains(sc[0], "+app_update 730") {
		t.Fatalf("steamcmd = %v", sc)
	}
}

// A generated config must always be parseable, even when a secret contains the
// characters that broke the old string-templated installer.
func TestAgentAndPanelConfigJSON(t *testing.T) {
	p := DefaultPlan()
	nasty := `p@ss"word\with$stuff`

	raw, err := AgentConfig(p, nasty, nasty, "")
	if err != nil {
		t.Fatal(err)
	}
	var agent map[string]any
	if err := json.Unmarshal([]byte(raw), &agent); err != nil {
		t.Fatalf("agent.json invalid: %v\n%s", err, raw)
	}
	if agent["token"] != nasty || agent["rcon_password"] != nasty {
		t.Fatalf("secrets mangled: %v", agent)
	}
	if agent["listen"] != "127.0.0.1:8100" {
		t.Fatalf("agent must stay on loopback: %v", agent["listen"])
	}
	if _, ok := agent["wp_dsn"]; ok {
		t.Fatal("wp_dsn must be absent when unset")
	}

	dsn := "cs2a:pw@tcp(127.0.0.1:3306)/cs2_wp"
	raw, err = AgentConfig(p, "tok", "rcon", dsn)
	if err != nil {
		t.Fatal(err)
	}
	agent = nil
	if err := json.Unmarshal([]byte(raw), &agent); err != nil {
		t.Fatalf("agent.json with wp_dsn invalid: %v\n%s", err, raw)
	}
	if agent["wp_dsn"] != dsn {
		t.Fatalf("wp_dsn = %v", agent["wp_dsn"])
	}

	raw, err = PanelConfig(p, "tok")
	if err != nil {
		t.Fatal(err)
	}
	var panel map[string]any
	if err := json.Unmarshal([]byte(raw), &panel); err != nil {
		t.Fatalf("panel.json invalid: %v\n%s", err, raw)
	}
	if panel["agent_url"] != "http://127.0.0.1:8100" {
		t.Fatalf("agent_url = %v", panel["agent_url"])
	}
	if _, ok := panel["public_url"]; ok {
		t.Fatal("public_url must be absent without a domain")
	}

	p.Domain = "panel.example.com"
	raw, _ = PanelConfig(p, "tok")
	panel = nil
	_ = json.Unmarshal([]byte(raw), &panel)
	if panel["public_url"] != "https://panel.example.com" {
		t.Fatalf("public_url = %v", panel["public_url"])
	}
}

// The proxy in front of the panel must not time out a plugin install.
func TestCaddySiteTimeouts(t *testing.T) {
	p := DefaultPlan()
	p.Domain = "panel.example.com"
	site := CaddySite(p)
	for _, want := range []string{
		"panel.example.com {",
		"reverse_proxy 127.0.0.1:8080",
		"response_header_timeout 10m",
		"flush_interval -1",
	} {
		if !strings.Contains(site, want) {
			t.Errorf("site block missing %q:\n%s", want, site)
		}
	}
}

func TestSplitHostPort(t *testing.T) {
	host, port := SplitHostPort("127.0.0.1:8100")
	if host != "127.0.0.1" || port != "8100" {
		t.Fatalf("got %s %s", host, port)
	}
}
