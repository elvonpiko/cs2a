package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeService implements ServiceController for tests.
type fakeService struct {
	mu      sync.Mutex
	active  bool
	enabled bool
	ops     []string
	// dieOnStart makes the unit exit right after a start/restart, the way a
	// CS2 server with bad launch args does.
	dieOnStart bool
	// journal is returned by JournalTail.
	journal []string
}

func (f *fakeService) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "start")
	f.active = !f.dieOnStart
	return nil
}
func (f *fakeService) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "stop")
	f.active = false
	return nil
}
func (f *fakeService) Restart() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "restart")
	f.active = !f.dieOnStart
	return nil
}
func (f *fakeService) IsActive() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active, nil
}
func (f *fakeService) IsEnabled() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled, nil
}
func (f *fakeService) UptimeSeconds() (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return 3600, f.active
}
func (f *fakeService) JournalTail(int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.journal == nil {
		return []string{"cs2-server: exited with code 1"}, nil
	}
	return append([]string(nil), f.journal...), nil
}

// fakeRCON is a minimal RCON server that records executed commands.
type fakeRCON struct {
	t        *testing.T
	ln       net.Listener
	password string
	mu       sync.Mutex
	commands []string
	wg       sync.WaitGroup
}

func startFakeRCON(t *testing.T, password string, responses map[string]string) *fakeRCON {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRCON{t: t, ln: ln, password: password}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				f.serve(conn, responses)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); f.wg.Wait() })
	return f
}

func (f *fakeRCON) addr() string { return f.ln.Addr().String() }

func (f *fakeRCON) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.commands...)
}

func (f *fakeRCON) serve(conn net.Conn, responses map[string]string) {
	defer conn.Close()
	authed := false
	for {
		var lenBuf [4]byte
		if _, err := readFullRCON(conn, lenBuf[:]); err != nil {
			return
		}
		size := binary.LittleEndian.Uint32(lenBuf[:])
		if size < 10 || size > 8192 {
			return
		}
		payload := make([]byte, size)
		if _, err := readFullRCON(conn, payload); err != nil {
			return
		}
		id := int32(binary.LittleEndian.Uint32(payload[0:4]))
		typ := int32(binary.LittleEndian.Uint32(payload[4:8]))
		body := string(payload[8 : len(payload)-2])
		switch typ {
		case 3: // auth
			if body != f.password {
				f.write(conn, -1, 2, "")
				return
			}
			f.write(conn, id, 0, "")
			f.write(conn, id, 2, "")
			authed = true
		case 2: // exec
			if !authed {
				return
			}
			f.mu.Lock()
			f.commands = append(f.commands, body)
			f.mu.Unlock()
			f.write(conn, id, 0, responses[body])
		case 0:
			// The client's end-of-response marker. A real SRCDS mirrors the
			// bogus request back, which is how a client knows a split response
			// has finished; answering it here keeps these tests on the same code
			// path as production instead of the slower idle-window fallback.
			if authed {
				f.write(conn, id, 0, "")
			}
		}
	}
}

func (f *fakeRCON) write(conn net.Conn, id, typ int32, body string) {
	size := 4 + 4 + len(body) + 2
	buf := make([]byte, 4+size)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(size))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(typ))
	copy(buf[12:], body)
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(buf)
}

func readFullRCON(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		nn, err := conn.Read(buf[n:])
		n += nn
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// newTestServer wires a Server against fakes.
func newTestServer(t *testing.T, responses map[string]string) (*Server, *fakeService, *fakeRCON, Config) {
	t.Helper()
	cfg := testConfig(t)
	svc := &fakeService{active: true}
	fake := startFakeRCON(t, "testpw", responses)
	cfg.RCONAddr = fake.addr()
	cfg.RCONPassword = "testpw"
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv := &Server{cfg: cfg, sysd: svc, store: store}
	return srv, svc, fake, cfg
}

func TestServerStatusStopped(t *testing.T) {
	srv, svc, _, _ := newTestServer(t, nil)
	svc.active = false
	st := srv.Status(context.Background())
	if st.Service.Active {
		t.Fatal("service should be inactive")
	}
	if st.Info != nil || st.Rcon != nil {
		t.Fatalf("queries should be skipped when stopped: %+v", st)
	}
}

func TestServerStatusRunning(t *testing.T) {
	statusOut := "hostname: test\nmap     : de_dust2\nplayers : 1 humans, 0 bots (12 max)\n\n# 2 \"Player One\" [U:1:1234567] 12:34 45 0 active\n"
	srv, _, fake, _ := newTestServer(t, map[string]string{"status": statusOut})
	st := srv.Status(context.Background())
	if !st.Service.Active {
		t.Fatal("service should be active")
	}
	// a2s unreachable in unit test — note set, rcon still parsed
	if st.Note == "" || !strings.Contains(st.Note, "a2s") {
		t.Fatalf("expected a2s note, got %q", st.Note)
	}
	if st.Rcon == nil || st.Rcon.Map != "de_dust2" || len(st.Rcon.Players) != 1 {
		t.Fatalf("rcon status = %+v", st.Rcon)
	}
	if len(fake.sent()) == 0 {
		t.Fatal("rcon commands not sent")
	}
}

// A lifecycle action must confirm the unit reached the requested state, and say
// what happened. Reporting success on the strength of systemctl's exit code is
// what left the operator unsure whether the buttons worked at all.
func TestServerLifecycleActions(t *testing.T) {
	srv, svc, _, _ := newTestServer(t, nil)
	for _, tc := range []struct {
		action Action
		active bool
	}{
		{ActionStart, true},
		{ActionRestart, true},
		{ActionStop, false},
	} {
		res := srv.Control(t.Context(), tc.action)
		if res.Failed {
			t.Fatalf("%s failed: %+v", tc.action, res)
		}
		if res.Active != tc.active {
			t.Fatalf("%s: active = %v, want %v", tc.action, res.Active, tc.active)
		}
		if res.Message == "" {
			t.Fatalf("%s produced no operator message", tc.action)
		}
	}
	ops := svc.ops
	if len(ops) != 3 || ops[0] != "start" || ops[1] != "restart" || ops[2] != "stop" {
		t.Fatalf("ops = %v", ops)
	}
}

// A unit that exits immediately must be reported as a failure, with the journal
// attached so the operator can see why.
func TestServerControlDetectsUnitThatDies(t *testing.T) {
	srv, svc, _, _ := newTestServer(t, nil)
	svc.dieOnStart = true
	svc.journal = []string{"cs2-server[42]: Fatal: could not bind port 27015"}

	res := srv.Control(t.Context(), ActionStart)
	if !res.Failed || res.Active {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Log) == 0 || !strings.Contains(res.Log[0], "could not bind") {
		t.Fatalf("journal not attached: %+v", res.Log)
	}
	if !strings.Contains(res.Message, "did not stay running") {
		t.Fatalf("message = %q", res.Message)
	}
}

// An unknown action is a programming error, not something to hand to systemd.
func TestServerControlRejectsUnknownAction(t *testing.T) {
	srv, svc, _, _ := newTestServer(t, nil)
	res := srv.Control(t.Context(), Action("nuke"))
	if !res.Failed {
		t.Fatalf("res = %+v", res)
	}
	if len(svc.ops) != 0 {
		t.Fatalf("systemd was called: %v", svc.ops)
	}
}

// Journal noise must not render as content: an empty log has to look empty.
func TestFilterJournalNoise(t *testing.T) {
	if got := filterJournalNoise([]string{"", "  ", "-- No entries --"}); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
	got := filterJournalNoise([]string{"", "real line", "-- No entries --"})
	if len(got) != 1 || got[0] != "real line" {
		t.Fatalf("got %v", got)
	}
}

func TestServerChangeMap(t *testing.T) {
	srv, _, fake, cfg := newTestServer(t, nil)
	mapsDir := filepath.Join(cfg.CSGODir(), "maps")
	if err := os.MkdirAll(mapsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"de_mirage.vpk", "de_dust2_dir.vpk", "junk.txt"} {
		if err := os.WriteFile(filepath.Join(mapsDir, m), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	maps, err := srv.Maps()
	if err != nil {
		t.Fatal(err)
	}
	if len(maps) != 2 || maps[0] != "de_dust2" || maps[1] != "de_mirage" {
		t.Fatalf("maps = %v", maps)
	}

	if err := srv.ChangeMap(context.Background(), "de_mirage"); err != nil {
		t.Fatal(err)
	}
	if err := srv.ChangeMap(context.Background(), "de_inferno"); err == nil {
		t.Fatal("expected unknown-map error")
	}
	if err := srv.ChangeMap(context.Background(), "Bad Map!"); err == nil {
		t.Fatal("expected invalid-name error")
	}
	sent := fake.sent()
	if len(sent) != 1 || sent[0] != "changelevel de_mirage" {
		t.Fatalf("sent = %v", sent)
	}
}

func TestServerSetPasswordAndSettings(t *testing.T) {
	srv, _, fake, cfg := newTestServer(t, nil)
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// pre-existing user content must survive
	userCfg := "hostname \"mine\"\n"
	if err := os.WriteFile(filepath.Join(cfg.CFGDir(), "server.cfg"), []byte(userCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := srv.SetPassword(context.Background(), "roundtrip"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(cfg.CFGDir(), "server.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(content)
	if !strings.Contains(s, `hostname "mine"`) || !strings.Contains(s, `sv_password "roundtrip"`) {
		t.Fatalf("cfg = %s", s)
	}
	sent := fake.sent()
	if len(sent) == 0 || sent[0] != `sv_password "roundtrip"` {
		t.Fatalf("sent = %v", sent)
	}

	// clearing password writes 0
	if err := srv.SetPassword(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	sent = fake.sent()
	if sent[len(sent)-1] != `sv_password "0"` {
		t.Fatalf("clear sent = %v", sent)
	}
}

func TestServerExecValidation(t *testing.T) {
	srv, _, fake, _ := newTestServer(t, nil)
	if _, err := srv.Exec(context.Background(), "   "); err == nil {
		t.Fatal("empty command should fail")
	}
	if _, err := srv.Exec(context.Background(), strings.Repeat("x", 600)); err == nil {
		t.Fatal("overlong command should fail")
	}
	if _, err := srv.Exec(context.Background(), "mp_warmuptime 5"); err != nil {
		t.Fatal(err)
	}
	if got := fake.sent(); len(got) != 1 || got[0] != "mp_warmuptime 5" {
		t.Fatalf("sent = %v", got)
	}
}

func TestWhitelistFile(t *testing.T) {
	_, _, _, cfg := newTestServer(t, nil)
	w := NewWhitelist(cfg)
	ids, err := w.Apply([]string{"[U:1:1234567]", "STEAM_1:0:11101", "[U:1:1234567]"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v", ids)
	}
	raw, _ := os.ReadFile(w.Path())
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	var lines []string
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		// the plugin accepts #, // and ; as comment markers
		if l == "" || strings.HasPrefix(l, "#") ||
			strings.HasPrefix(l, "//") || strings.HasPrefix(l, ";") {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) != 2 {
		t.Fatalf("file lines = %v", lines)
	}
	got, err := w.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read = %v", got)
	}
	// invalid entry rejected
	if _, err := w.Apply([]string{"not-a-steamid"}); err == nil {
		t.Fatal("expected invalid steamid error")
	}
}

// Enforcement lives in the plugin's core.cfg KeyValues file, not in a cvar.
func TestWhitelistEnableSwitch(t *testing.T) {
	_, _, _, cfg := newTestServer(t, nil)
	w := NewWhitelist(cfg)

	// no config yet => not enforcing, and no error
	on, err := w.Enabled()
	if err != nil || on {
		t.Fatalf("fresh state: on=%v err=%v", on, err)
	}

	// enforcement requires somebody on the list (see SetEnabled)
	if _, err := w.Apply([]string{"[U:1:1234567]"}); err != nil {
		t.Fatal(err)
	}

	// enabling with no config writes a usable default
	if err := w.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	on, err = w.Enabled()
	if err != nil || !on {
		t.Fatalf("after enable: on=%v err=%v", on, err)
	}
	raw, _ := os.ReadFile(w.CorePath())
	if !strings.Contains(string(raw), `"Filename"`) {
		t.Fatalf("core.cfg missing Filename key:\n%s", raw)
	}

	// toggling off must preserve every other key and comment
	if err := w.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	on, _ = w.Enabled()
	if on {
		t.Fatal("still enforcing after disable")
	}
	raw2, _ := os.ReadFile(w.CorePath())
	if !strings.Contains(string(raw2), `"Immunity"`) || !strings.Contains(string(raw2), "cs2a") {
		t.Fatalf("other config content lost:\n%s", raw2)
	}
}

// An operator's hand-written core.cfg must keep its own settings; only the
// Enable value may change.
func TestWhitelistEnablePreservesOperatorConfig(t *testing.T) {
	_, _, _, cfg := newTestServer(t, nil)
	w := NewWhitelist(cfg)
	if _, err := w.Apply([]string{"[U:1:1234567]"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(w.CorePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	custom := `"cs2whitelist"
{
	"Config"
	{
		"Enable"	"0"
		"Filename"	"custom.txt"
		"LogToFile"	"1"
	}
}
`
	if err := os.WriteFile(w.CorePath(), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(w.CorePath())
	s := string(raw)
	if !strings.Contains(s, `"Enable"	"1"`) {
		t.Fatalf("Enable not flipped:\n%s", s)
	}
	if !strings.Contains(s, `"custom.txt"`) || !strings.Contains(s, `"LogToFile"`) {
		t.Fatalf("operator settings lost:\n%s", s)
	}
}

// Installing the whitelist plugin must not start enforcing. It used to write
// Enable "1" while whitelist.txt did not exist yet, so the next restart rejected
// every connection — installing a plugin locked the operator out of their own
// server.
func TestWhitelistInstallDoesNotEnforce(t *testing.T) {
	_, _, _, cfg := newTestServer(t, nil)
	w := NewWhitelist(cfg)
	if err := writeWhitelistCoreCFG(w.CorePath()); err != nil {
		t.Fatal(err)
	}
	on, err := w.Enabled()
	if err != nil {
		t.Fatal(err)
	}
	if on {
		raw, _ := os.ReadFile(w.CorePath())
		t.Fatalf("a fresh install must not enforce the whitelist:\n%s", raw)
	}
	// The file the panel manages must still be named, so switching enforcement
	// on later needs no hand-editing.
	raw, _ := os.ReadFile(w.CorePath())
	if !strings.Contains(string(raw), `"Filename"`) {
		t.Fatalf("core.cfg must point at the managed list:\n%s", raw)
	}
}

// Refusing to enforce an empty list is the agent's job, not just the panel's:
// the panel hides the button, and this is what makes the rule hold for any
// caller. The mirror case matters too — emptying the list while enforcement is
// already on has exactly the same effect.
func TestWhitelistRefusesEnforcingEmptyList(t *testing.T) {
	_, _, _, cfg := newTestServer(t, nil)
	w := NewWhitelist(cfg)
	err := w.SetEnabled(true)
	if err == nil {
		t.Fatal("enforcing an empty whitelist must be refused")
	}
	if !strings.Contains(err.Error(), "empty whitelist") {
		t.Fatalf("error should name the problem, got %v", err)
	}
	if on, _ := w.Enabled(); on {
		t.Fatal("enforcement was switched on despite the refusal")
	}
	// Disabling is always allowed, even with nothing on the list.
	if err := w.SetEnabled(false); err != nil {
		t.Fatalf("disable must always work: %v", err)
	}

	// Now enforce properly, then try to empty the list.
	if _, err := w.Apply([]string{"[U:1:1234567]"}); err != nil {
		t.Fatal(err)
	}
	if err := w.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(nil); err == nil {
		t.Fatal("clearing an enforced whitelist must be refused")
	}
	if ids, _ := w.Read(); len(ids) != 1 {
		t.Fatalf("the list must be untouched, got %v", ids)
	}
	// With enforcement off, clearing it is fine.
	if err := w.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(nil); err != nil {
		t.Fatalf("clearing an unenforced list must work: %v", err)
	}
}
