package rcon

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a minimal Source RCON server implementing enough of the
// protocol for tests: AUTH handshake and command exec with canned responses.
type fakeServer struct {
	t        *testing.T
	ln       net.Listener
	password string

	mu        sync.Mutex
	responses map[string]string // command -> response
	wg        sync.WaitGroup    // waits for accept/conn goroutines
}

func newFakeServer(t *testing.T, password string) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeServer{t: t, ln: ln, password: password, responses: map[string]string{}}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) on(cmd, resp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[strings.TrimSpace(cmd)] = resp
}

func (s *fakeServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(conn)
		}()
	}
}

func (s *fakeServer) serveConn(conn net.Conn) {
	defer conn.Close()
	authed := false
	for {
		size, err := readUint32(conn)
		if err != nil {
			return
		}
		if size < 10 || size > 8192 {
			return
		}
		payload := make([]byte, size)
		if _, err := readFull(conn, payload); err != nil {
			return
		}
		id := int32(binary.LittleEndian.Uint32(payload[0:4]))
		typ := int32(binary.LittleEndian.Uint32(payload[4:8]))
		body := payload[8 : len(payload)-2]

		switch typ {
		case typeServerDataAuth:
			if string(body) != s.password {
				// spec: empty RESPONSE_VALUE, then AUTH_RESPONSE with id -1
				s.write(conn, id, typeServerDataResponseValue, "")
				s.write(conn, -1, typeServerDataAuthResponse, "")
				return
			}
			s.write(conn, id, typeServerDataResponseValue, "")
			s.write(conn, id, typeServerDataAuthResponse, "")
			authed = true
		case typeServerDataExecCommand:
			if !authed {
				return
			}
			s.mu.Lock()
			resp := s.responses[strings.TrimSpace(string(body))]
			s.mu.Unlock()
			s.write(conn, id, typeServerDataResponseValue, resp)
		default:
			// ignore unknown packet types
		}
	}
}

func (s *fakeServer) write(conn net.Conn, id, typ int32, body string) {
	s.t.Helper()
	// Real servers split large responses into ~4096-byte packets; do the same
	// so the client's reassembly logic is exercised.
	const chunk = 4096
	for len(body) > chunk {
		s.writePacket(conn, id, typ, body[:chunk])
		body = body[chunk:]
	}
	s.writePacket(conn, id, typ, body)
}

func (s *fakeServer) writePacket(conn net.Conn, id, typ int32, body string) {
	s.t.Helper()
	size := 4 + 4 + len(body) + 2
	buf := make([]byte, 4+size)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(size))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(typ))
	copy(buf[12:], body)
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(buf); err != nil {
		s.t.Logf("fakeServer write: %v", err)
	}
}

func readUint32(conn net.Conn) (uint32, error) {
	var b [4]byte
	if _, err := readFull(conn, b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
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

// --- tests ------------------------------------------------------------

func TestDialAuthOk(t *testing.T) {
	s := newFakeServer(t, "sekrit")
	c, err := Dial(s.addr(), "sekrit", 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
}

func TestDialAuthRejected(t *testing.T) {
	s := newFakeServer(t, "sekrit")
	_, err := Dial(s.addr(), "wrong", 2*time.Second)
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("want ErrAuthFailed, got %v", err)
	}
}

func TestDialUnreachable(t *testing.T) {
	// port 1 on loopback should be closed
	_, err := Dial("127.0.0.1:1", "pw", 500*time.Millisecond)
	if err == nil {
		t.Fatal("dial to closed port should fail")
	}
}

func TestExecSinglePacket(t *testing.T) {
	s := newFakeServer(t, "pw")
	s.on("status", "hostname: test\nplayers : 3")
	c, err := Dial(s.addr(), "pw", 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	out, err := c.Exec("status")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if want := "hostname: test\nplayers : 3"; out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestExecMultiPacket(t *testing.T) {
	s := newFakeServer(t, "pw")
	s.on("cvarlist", strings.Repeat("a fake cvar line\n", 400)) // > 4096 bytes
	c, err := Dial(s.addr(), "pw", 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	out, err := c.Exec("cvarlist")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(out, "a fake cvar line") || len(out) < 4096 {
		t.Fatalf("multi-packet response truncated: %d bytes", len(out))
	}
}

func TestExecEmptyResponse(t *testing.T) {
	s := newFakeServer(t, "pw")
	c, err := Dial(s.addr(), "pw", 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	out, err := c.Exec("mp_warmuptime 5")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out != "" {
		t.Fatalf("want empty response, got %q", out)
	}
}

func TestPacketSizeMath(t *testing.T) {
	// sanity: a 4096-byte body must produce a size field of 4106
	body := strings.Repeat("x", 4096)
	if size := 4 + 4 + len(body) + 2; size != 4106 {
		t.Fatalf("size = %d, want 4106", size)
	}
	if maxPacketSize != 4113 {
		t.Fatalf("maxPacketSize = %d, want 4113", maxPacketSize)
	}
	if !strings.Contains(fmt.Sprint(maxPacketSize), "4113") {
		t.Fatal("unreachable")
	}
}
