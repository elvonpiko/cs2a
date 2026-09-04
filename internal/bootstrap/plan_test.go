package bootstrap

import (
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
		"+ip 0.0.0.0",
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
	if len(fw) != 2 || !strings.Contains(fw[0], "27015/tcp") || !strings.Contains(fw[1], "27015/udp") {
		t.Fatalf("fw = %v", fw)
	}
	sc := SteamCMDCmds(p)
	if !strings.Contains(sc[0], "+force_install_dir /opt/cs2a/cs2") || !strings.Contains(sc[0], "+app_update 730") {
		t.Fatalf("steamcmd = %v", sc)
	}
}

func TestSplitHostPort(t *testing.T) {
	host, port := SplitHostPort("127.0.0.1:8100")
	if host != "127.0.0.1" || port != "8100" {
		t.Fatalf("got %s %s", host, port)
	}
}
