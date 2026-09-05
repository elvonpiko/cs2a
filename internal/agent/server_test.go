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
}

func (f *fakeService) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "start")
	f.active = true
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

func TestServerLifecycleActions(t *testing.T) {
	srv, svc, _, _ := newTestServer(t, nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Restart(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
	ops := svc.ops
	if len(ops) != 3 || ops[0] != "start" || ops[1] != "restart" || ops[2] != "stop" {
		t.Fatalf("ops = %v", ops)
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
