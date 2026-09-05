package agent

import (
	"encoding/binary"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stallingRCON answers the first command with one full-size packet and then goes
// quiet. On the wire that is indistinguishable from "more is coming", which is
// exactly the case the client used to report as a complete, successful answer.
type stallingRCON struct {
	ln       net.Listener
	password string
	wg       sync.WaitGroup
}

func startStallingRCON(t *testing.T, password string) *stallingRCON {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &stallingRCON{ln: ln, password: password}
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
				f.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); f.wg.Wait() })
	return f
}

func (f *stallingRCON) addr() string { return f.ln.Addr().String() }

func (f *stallingRCON) serve(conn net.Conn) {
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
		case 3:
			if body != f.password {
				return
			}
			f.write(conn, id, 0, "")
			f.write(conn, id, 2, "")
			authed = true
		case 2:
			if !authed {
				return
			}
			// A body at the 4096-byte maximum is the engine's way of saying the
			// answer continues in the next packet. Sending one and then nothing
			// leaves the client waiting for a continuation that never comes.
			f.write(conn, id, 0, strings.Repeat("a", 4096))
			// Deliberately never answer the end marker, and hold the connection
			// open past the client's split-continuation window so a close cannot
			// be mistaken for the end of the output.
			time.Sleep(3 * time.Second)
			return
		}
	}
}

func (f *stallingRCON) write(conn net.Conn, id, typ int32, body string) {
	size := 4 + 4 + len(body) + 2
	buf := make([]byte, 4+size)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(size))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(typ))
	copy(buf[12:], body)
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(buf)
}

// A cut-short RCON answer must reach the operator as partial data plus an
// explicit note, never as a complete response. The console showed half a
// cvarlist with no indication that the rest was missing.
func TestAPIExecReportsATruncatedAnswer(t *testing.T) {
	cfg := testConfig(t)
	svc := &fakeService{active: true}
	fake := startStallingRCON(t, "testpw")
	cfg.RCONAddr = fake.addr()
	cfg.RCONPassword = "testpw"
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv := &Server{cfg: cfg, sysd: svc, store: store}
	gh := NewGHClient("")
	gh.HTTP.Transport = offlineTransport{}
	lo := NewLoadoutStore(cfg, store)
	t.Cleanup(lo.Close)
	api := NewAPI(cfg, srv, NewWhitelist(cfg), NewInstaller(cfg, store, DefaultCatalog(), gh), lo)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	client := newAuthClient(cfg.Token)

	resp, out := doJSON(t, client, "POST", ts.URL, "/api/v1/server/exec", map[string]any{"command": "cvarlist"})
	if resp.StatusCode != 200 {
		t.Fatalf("exec: %d %v", resp.StatusCode, out)
	}
	if out["truncated"] != true {
		t.Fatalf("a cut-short answer must be flagged: %v", out)
	}
	text, _ := out["output"].(string)
	if len(text) < 4096 {
		t.Errorf("the partial output must still be returned, got %d bytes", len(text))
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "truncated") {
		t.Errorf("note = %q", note)
	}
}
