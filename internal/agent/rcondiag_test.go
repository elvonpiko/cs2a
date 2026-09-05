package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A server adopted from an existing install may bind an explicit -ip, which is
// the actual cause of the "dial 127.0.0.1:27015: connection refused" the panel
// used to report with no explanation.
func TestParseLaunchLine(t *testing.T) {
	cases := []struct {
		name    string
		line    string
		ip      string
		port    int
		usercon bool
		host    string
	}{
		{
			name:    "wildcard bind is reachable on loopback",
			line:    "/opt/cs2/game/cs2.sh -dedicated -console -usercon -ip 0.0.0.0 -port 27015 +map de_dust2",
			ip:      "0.0.0.0",
			port:    27015,
			usercon: true,
			host:    "127.0.0.1",
		},
		{
			name:    "explicit public bind must be dialled directly",
			line:    "/home/steam/cs2/game/cs2.sh -dedicated -usercon -ip 203.0.113.7 -port 27020",
			ip:      "203.0.113.7",
			port:    27020,
			usercon: true,
			host:    "203.0.113.7",
		},
		{
			name: "no usercon means the engine never opens the port",
			line: "/opt/cs2/game/cs2.sh -dedicated -console -port 27015",
			port: 27015,
			host: "127.0.0.1",
		},
		{
			name:    "equals form and quoting",
			line:    `/opt/cs2/game/cs2.sh -dedicated "-usercon" -ip="10.0.0.5" -port=27016`,
			ip:      "10.0.0.5",
			port:    27016,
			usercon: true,
			host:    "10.0.0.5",
		},
		{
			name:    "plus-prefixed port",
			line:    "/opt/cs2/game/cs2.sh -dedicated -usercon +hostport 27018",
			port:    27018,
			usercon: true,
			host:    "127.0.0.1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLaunchLine(tc.line)
			if !got.Found {
				t.Fatal("launch line not parsed")
			}
			if got.IP != tc.ip || got.Port != tc.port || got.UserCon != tc.usercon {
				t.Fatalf("got ip=%q port=%d usercon=%v", got.IP, got.Port, got.UserCon)
			}
			if got.RCONHost() != tc.host {
				t.Fatalf("RCONHost = %q, want %q", got.RCONHost(), tc.host)
			}
		})
	}
	if parseLaunchLine("  ").Found {
		t.Fatal("empty line must not report Found")
	}
}

// systemctl show emits ExecStart as a structured record; a raw unit file emits
// a plain command. Both must yield the launch line.
func TestParseExecStartProperty(t *testing.T) {
	structured := `ExecStart={ path=/opt/cs2/game/cs2.sh ; argv[]=/opt/cs2/game/cs2.sh -dedicated -usercon -port 27015 ; ignore_errors=no ; start_time=[n/a] }`
	if got := parseExecStartProperty(structured); got != "/opt/cs2/game/cs2.sh -dedicated -usercon -port 27015" {
		t.Fatalf("structured: %q", got)
	}
	plain := "ExecStart=/opt/cs2/game/cs2.sh -dedicated -port 27015"
	if got := parseExecStartProperty(plain); got != "/opt/cs2/game/cs2.sh -dedicated -port 27015" {
		t.Fatalf("plain: %q", got)
	}
	if got := parseExecStartProperty(""); got != "" {
		t.Fatalf("empty: %q", got)
	}
}

// launchService is a fakeService that also reports a launch line and records
// drop-ins, so the RCON diagnosis and repair paths are testable.
type launchService struct {
	fakeService
	args    LaunchArgs
	dropIns map[string]string
}

func (l *launchService) UnitLaunchArgs() LaunchArgs { return l.args }

func (l *launchService) WriteDropIn(name, content string) error {
	if l.dropIns == nil {
		l.dropIns = map[string]string{}
	}
	l.dropIns[name] = content
	// A drop-in only takes effect on the next start, exactly like systemd.
	return nil
}

func newDiagServer(t *testing.T, cfg Config, args LaunchArgs) (*Server, *launchService) {
	t.Helper()
	svc := &launchService{fakeService: fakeService{active: true}, args: args}
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &Server{cfg: cfg, sysd: svc, store: store}, svc
}

// The single most confusing failure this panel had: the agent dials loopback
// while the game binds a public address. The diagnosis must say so and offer
// the address that would work.
func TestDiagnoseRCONDetectsAddressMismatch(t *testing.T) {
	cfg := testConfig(t)
	cfg.RCONAddr = "127.0.0.1:27015"
	cfg.RCONPassword = "pw"
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CFGDir(), "server.cfg"), []byte("rcon_password \"pw\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := newDiagServer(t, cfg, parseLaunchLine(
		"/opt/cs2/game/cs2.sh -dedicated -usercon -ip 203.0.113.7 -port 27015"))

	d := srv.DiagnoseRCON()
	if d.OK {
		t.Fatal("nothing is listening; OK must be false")
	}
	if d.SuggestedAddr != "203.0.113.7:27015" {
		t.Fatalf("SuggestedAddr = %q", d.SuggestedAddr)
	}
	if !d.Repairable {
		t.Fatal("an address mismatch is repairable")
	}
	if !strings.Contains(d.Reason, "203.0.113.7:27015") || !strings.Contains(d.Reason, "127.0.0.1:27015") {
		t.Fatalf("reason should contrast both addresses: %q", d.Reason)
	}
}

// CS2 opens the RCON port only when -usercon is on the command line. The
// installer merely warned about this in a wall of log output.
func TestDiagnoseRCONDetectsMissingUserCon(t *testing.T) {
	cfg := testConfig(t)
	cfg.RCONAddr = "127.0.0.1:27015"
	cfg.RCONPassword = "pw"
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CFGDir(), "server.cfg"), []byte("rcon_password \"pw\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := newDiagServer(t, cfg, parseLaunchLine(
		"/opt/cs2/game/cs2.sh -dedicated -console -ip 0.0.0.0 -port 27015"))

	d := srv.DiagnoseRCON()
	if !d.MissingUserCon || d.MissingPassword {
		t.Fatalf("diagnosis = %+v", d)
	}
	if !strings.Contains(d.Reason, "-usercon") {
		t.Fatalf("reason = %q", d.Reason)
	}
	if !strings.Contains(d.Fix, "restart") {
		t.Fatalf("the fix only applies at boot, so it must mention a restart: %q", d.Fix)
	}
}

// The installer writes rcon_password into server.cfg but cannot restart a
// server it did not start; until then the engine has no password and keeps the
// port closed. The agent knowing a password is not the same as the game knowing
// it.
func TestDiagnoseRCONDetectsPasswordNeverGivenToTheGame(t *testing.T) {
	cfg := testConfig(t)
	cfg.RCONAddr = "127.0.0.1:27015"
	cfg.RCONPassword = "pw"
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// server.cfg exists but has no rcon_password
	if err := os.WriteFile(filepath.Join(cfg.CFGDir(), "server.cfg"), []byte("hostname \"mine\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := newDiagServer(t, cfg, parseLaunchLine(
		"/opt/cs2/game/cs2.sh -dedicated -usercon -ip 0.0.0.0 -port 27015"))

	d := srv.DiagnoseRCON()
	if !d.MissingPassword {
		t.Fatalf("diagnosis = %+v", d)
	}
	if !strings.Contains(d.Reason, "rcon_password") {
		t.Fatalf("reason = %q", d.Reason)
	}
}

// A commented-out password must not count as configured.
func TestServerCFGHasRCONPasswordIgnoresComments(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	srv, _ := newDiagServer(t, cfg, LaunchArgs{})
	path := filepath.Join(cfg.CFGDir(), "server.cfg")

	for _, tc := range []struct {
		body string
		want bool
	}{
		{"// rcon_password \"pw\"\n", false},
		{"rcon_password \"\"\n", false},
		{"rcon_password \"pw\"\n", true},
		{"  rcon_password pw\n", true},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := srv.serverCFGHasRCONPassword(); got != tc.want {
			t.Fatalf("%q -> %v, want %v", tc.body, got, tc.want)
		}
	}
}

// Repair writes the password into the managed block and adds -usercon through a
// drop-in, leaving the operator's own unit file untouched.
func TestRepairRCONWritesPasswordAndUserCon(t *testing.T) {
	cfg := testConfig(t)
	cfg.RCONAddr = "127.0.0.1:27015"
	cfg.RCONPassword = "pw"
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CFGDir(), "server.cfg"), []byte("hostname \"mine\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	launch := "/opt/cs2/game/cs2.sh -dedicated -console -ip 0.0.0.0 -port 27015"
	srv, svc := newDiagServer(t, cfg, parseLaunchLine(launch))

	applied, res, err := srv.RepairRCON(t.Context())
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied = %v", applied)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.CFGDir(), "server.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "rcon_password \"pw\"") {
		t.Fatalf("server.cfg not updated:\n%s", raw)
	}
	if !strings.Contains(string(raw), "hostname \"mine\"") {
		t.Fatalf("the operator's own settings were lost:\n%s", raw)
	}
	dropIn := svc.dropIns[userConDropIn]
	if !strings.Contains(dropIn, "ExecStart=\n") {
		t.Fatalf("a drop-in must clear ExecStart before redefining it:\n%s", dropIn)
	}
	if !strings.Contains(dropIn, launch+" -usercon") {
		t.Fatalf("drop-in did not preserve the original launch line:\n%s", dropIn)
	}
	// Both fixes only apply at boot, so the server must have been restarted.
	if res.Action != "restart" {
		t.Fatalf("result = %+v", res)
	}
	// Applying the repair twice must not stack duplicate cfg lines.
	if _, _, err := srv.RepairRCON(t.Context()); err != nil {
		t.Fatalf("second repair: %v", err)
	}
	raw2, _ := os.ReadFile(filepath.Join(cfg.CFGDir(), "server.cfg"))
	if n := strings.Count(string(raw2), "rcon_password"); n != 1 {
		t.Fatalf("rcon_password appears %d times:\n%s", n, raw2)
	}
}

// An address correction is persisted so the next agent restart keeps working.
func TestRepairRCONPersistsCorrectedAddress(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.json")
	cfg := testConfig(t)
	cfg.RCONAddr = "127.0.0.1:27015"
	cfg.A2SAddr = "127.0.0.1:27015"
	cfg.RCONPassword = "pw"
	cfg.path = cfgPath
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CFGDir(), "server.cfg"), []byte("rcon_password \"pw\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := newDiagServer(t, cfg, parseLaunchLine(
		"/opt/cs2/game/cs2.sh -dedicated -usercon -ip 203.0.113.7 -port 27020"))

	if _, _, err := srv.RepairRCON(t.Context()); err != nil {
		t.Fatalf("repair: %v", err)
	}
	if srv.cfg.RCONAddr != "203.0.113.7:27020" {
		t.Fatalf("RCONAddr = %q", srv.cfg.RCONAddr)
	}
	if srv.cfg.A2SAddr != "203.0.113.7:27020" {
		t.Fatalf("A2SAddr should follow the game's bind: %q", srv.cfg.A2SAddr)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config was not persisted: %v", err)
	}
	if !strings.Contains(string(raw), "203.0.113.7:27020") {
		t.Fatalf("persisted config missing the corrected address:\n%s", raw)
	}
	// The file holds the agent token and RCON password.
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %o, want 600", perm)
	}
}

// When RCON works there is nothing to repair and nothing to restart.
func TestDiagnoseRCONOKWhenReachable(t *testing.T) {
	cfg := testConfig(t)
	fake := startFakeRCON(t, "pw", nil)
	cfg.RCONAddr = fake.addr()
	cfg.RCONPassword = "pw"
	srv, svc := newDiagServer(t, cfg, parseLaunchLine(
		"/opt/cs2/game/cs2.sh -dedicated -usercon -port 27015"))

	if d := srv.DiagnoseRCON(); !d.OK || d.Reason != "" {
		t.Fatalf("diagnosis = %+v", d)
	}
	applied, res, err := srv.RepairRCON(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 || res.Failed {
		t.Fatalf("nothing should have been applied: %v %+v", applied, res)
	}
	if len(svc.dropIns) != 0 {
		t.Fatalf("a working server must not be modified: %v", svc.dropIns)
	}
	if got := svc.ops; len(got) != 0 {
		t.Fatalf("a working server must not be restarted: %v", got)
	}
}

// A running server that rejects the agent's password is a different problem from
// an unreachable port: the listener is open, -usercon is on, server.cfg has a
// password — just not this one. The diagnosis used to fall through to "give the
// server a minute to finish loading the map", so the single most common
// misconfiguration was reported as impatience.
func TestDiagnoseRCONDetectsRejectedPassword(t *testing.T) {
	cfg := testConfig(t)
	fake := startFakeRCON(t, "the-real-one", nil)
	cfg.RCONAddr = fake.addr()
	cfg.RCONPassword = "what-the-agent-thinks"
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// server.cfg does carry a password, so the "never told the game" finding
	// must not fire either.
	if err := os.WriteFile(filepath.Join(cfg.CFGDir(), "server.cfg"),
		[]byte("rcon_password \"the-real-one\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := newDiagServer(t, cfg, parseLaunchLine(
		"/opt/cs2/game/cs2.sh -dedicated -usercon -port 27015"))

	d := srv.DiagnoseRCON()
	if d.OK {
		t.Fatal("a rejected password is not a working connection")
	}
	if !d.PasswordMismatch {
		t.Fatalf("diagnosis = %+v", d)
	}
	if d.MissingUserCon || d.MissingPassword || d.SuggestedAddr != "" {
		t.Fatalf("invented an unrelated finding: %+v", d)
	}
	if !strings.Contains(d.Reason, "password") {
		t.Fatalf("reason = %q", d.Reason)
	}
	if strings.Contains(d.Fix, "give it a minute") || strings.Contains(d.Fix, "loading the map") {
		t.Fatalf("fix must not blame map loading: %q", d.Fix)
	}
	if !d.Repairable {
		t.Fatal("the agent has a password, so writing it into server.cfg is a real repair")
	}
}

// Repairing a rejected password writes the agent's password into server.cfg and
// restarts, because the engine only reads it at boot.
func TestRepairRCONFixesARejectedPassword(t *testing.T) {
	cfg := testConfig(t)
	fake := startFakeRCON(t, "the-real-one", nil)
	cfg.RCONAddr = fake.addr()
	cfg.RCONPassword = "agent-secret"
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.CFGDir(), "server.cfg"),
		[]byte("hostname \"mine\"\nrcon_password \"the-real-one\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, svc := newDiagServer(t, cfg, parseLaunchLine(
		"/opt/cs2/game/cs2.sh -dedicated -usercon -port 27015"))

	applied, res, err := srv.RepairRCON(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) == 0 {
		t.Fatalf("nothing applied: %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.CFGDir(), "server.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "agent-secret") {
		t.Fatalf("the agent's password was not written:\n%s", raw)
	}
	// The operator's own lines survive.
	if !strings.Contains(string(raw), `hostname "mine"`) {
		t.Fatalf("operator config lost:\n%s", raw)
	}
	if len(svc.ops) == 0 || svc.ops[len(svc.ops)-1] != "restart" {
		t.Fatalf("a password only takes effect at boot, so a restart is required: %v", svc.ops)
	}
}
