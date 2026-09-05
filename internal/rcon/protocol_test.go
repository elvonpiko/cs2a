package rcon

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// rawServer is a fake that writes exactly the bytes a test dictates, for the
// cases the well-behaved fakeServer cannot express (mid-response pauses, bad
// frames, servers that hang up).
type rawServer struct {
	t  *testing.T
	ln net.Listener
	// handle is called once per connection, after the client has authenticated.
	handle func(conn net.Conn)
}

func newRawServer(t *testing.T, handle func(conn net.Conn)) *rawServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &rawServer{t: t, ln: ln, handle: handle}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// AUTH handshake: accept any password.
		p, err := readFrame(conn)
		if err != nil {
			return
		}
		writeFrame(conn, p.id, typeServerDataResponseValue, nil)
		writeFrame(conn, p.id, typeServerDataAuthResponse, nil)
		s.handle(conn)
		// Drain whatever the client still sends (its end-marker request) until
		// it closes. Closing with unread data queued would send an RST and the
		// client would lose the answer it had already received.
		drain(conn)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return s
}

func (s *rawServer) addr() string { return s.ln.Addr().String() }

// drain reads until the peer closes, so the test server never resets a
// connection that still has unread client bytes queued.
func drain(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	for {
		if _, err := conn.Read(buf); err != nil {
			return
		}
	}
}

type frame struct {
	id   int32
	typ  int32
	body []byte
}

func readFrame(conn net.Conn) (frame, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return frame{}, err
	}
	size := binary.LittleEndian.Uint32(lenBuf[:])
	if size < 10 || size > 65536 {
		return frame{}, errors.New("bad size")
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return frame{}, err
	}
	return frame{
		id:   int32(binary.LittleEndian.Uint32(buf[0:4])),
		typ:  int32(binary.LittleEndian.Uint32(buf[4:8])),
		body: buf[8 : size-2],
	}, nil
}

func writeFrame(conn net.Conn, id, typ int32, body []byte) {
	size := 4 + 4 + len(body) + 2
	buf := make([]byte, 4+size)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(size))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(typ))
	copy(buf[12:], body)
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(buf)
}

// writeRawFrame writes a frame with an explicit size field, for framing tests.
func writeRawFrame(conn net.Conn, size uint32, id, typ int32, body []byte) {
	head := make([]byte, 12)
	binary.LittleEndian.PutUint32(head[0:4], size)
	binary.LittleEndian.PutUint32(head[4:8], uint32(id))
	binary.LittleEndian.PutUint32(head[8:12], uint32(typ))
	_, _ = conn.Write(append(head, body...))
}

func dialRaw(t *testing.T, addr string) *Client {
	t.Helper()
	c, err := Dial(addr, "pw", 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.ExecTimeout = 2 * time.Second
	c.ExecReadIdle = 50 * time.Millisecond
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// A server that pauses in the middle of a split response must not produce a
// short answer reported as success. The old client used a 350 ms idle window
// after the first packet, so a loaded or remote server silently lost the tail of
// `status` — and the panel parsed the fragment as the whole truth.
func TestExecReportsAPauseMidResponse(t *testing.T) {
	first := strings.Repeat("a", maxBodySize)
	s := newRawServer(t, func(conn net.Conn) {
		p, err := readFrame(conn)
		if err != nil {
			return
		}
		writeFrame(conn, p.id, typeServerDataResponseValue, []byte(first))
		time.Sleep(1500 * time.Millisecond) // longer than any idle window
		writeFrame(conn, p.id, typeServerDataResponseValue, []byte("the tail"))
	})
	c := dialRaw(t, s.addr())
	out, err := c.Exec("status")
	if err == nil {
		t.Fatalf("a truncated response was reported as success (%d bytes)", len(out))
	}
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
	if len(out) != len(first) {
		t.Errorf("collected %d bytes, want the %d received so far", len(out), len(first))
	}
}

// A response longer than the old 16-packet cap must arrive whole. `cvarlist` and
// `sm plugins list` exceed it, and the cap silently dropped everything past
// 64 KB with err == nil.
func TestExecReadsMoreThanSixteenPackets(t *testing.T) {
	const packets = 20
	s := newRawServer(t, func(conn net.Conn) {
		p, err := readFrame(conn)
		if err != nil {
			return
		}
		for i := 0; i < packets; i++ {
			writeFrame(conn, p.id, typeServerDataResponseValue, []byte(strings.Repeat("x", maxBodySize)))
		}
		writeFrame(conn, p.id, typeServerDataResponseValue, []byte("end"))
	})
	c := dialRaw(t, s.addr())
	out, err := c.Exec("cvarlist")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if want := packets*maxBodySize + len("end"); len(out) != want {
		t.Fatalf("got %d bytes, want %d", len(out), want)
	}
}

// Frames that fail the id/type check must not be counted as the answer. A server
// answering with a mismatched id used to yield ("", nil) — an empty response
// indistinguishable from a command that legitimately prints nothing.
func TestExecIgnoresForeignPacketsInsteadOfSucceedingEmpty(t *testing.T) {
	s := newRawServer(t, func(conn net.Conn) {
		p, err := readFrame(conn)
		if err != nil {
			return
		}
		// Three frames with the wrong id, then the real answer.
		for i := 0; i < 3; i++ {
			writeFrame(conn, 9999, typeServerDataResponseValue, []byte("not yours"))
		}
		writeFrame(conn, p.id, typeServerDataResponseValue, []byte("hostname: real"))
	})
	c := dialRaw(t, s.addr())
	out, err := c.Exec("status")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out != "hostname: real" {
		t.Fatalf("out = %q", out)
	}
}

// An empty packet in front of the real output must not end the response: the old
// client treated any empty body as the terminator and returned "".
func TestExecSkipsALeadingEmptyPacket(t *testing.T) {
	s := newRawServer(t, func(conn net.Conn) {
		p, err := readFrame(conn)
		if err != nil {
			return
		}
		writeFrame(conn, p.id, typeServerDataResponseValue, nil)
		writeFrame(conn, p.id, typeServerDataResponseValue, []byte("real output"))
	})
	c := dialRaw(t, s.addr())
	out, err := c.Exec("status")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out != "real output" {
		t.Fatalf("out = %q — the leading empty ack was mistaken for the terminator", out)
	}
}

// A command that genuinely produces no output still returns promptly: the
// terminator after collected output ends the response.
func TestExecEmptyTerminatorAfterOutput(t *testing.T) {
	s := newRawServer(t, func(conn net.Conn) {
		p, err := readFrame(conn)
		if err != nil {
			return
		}
		writeFrame(conn, p.id, typeServerDataResponseValue, []byte("done"))
		writeFrame(conn, p.id, typeServerDataResponseValue, nil)
		time.Sleep(2 * time.Second) // must not be waited for
	})
	c := dialRaw(t, s.addr())
	c.ExecTimeout = 5 * time.Second
	start := time.Now()
	out, err := c.Exec("mp_warmuptime 5")
	if err != nil || out != "done" {
		t.Fatalf("out = %q err = %v", out, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v; the terminator was ignored", elapsed)
	}
}

// Output already received must not be thrown away when the server hangs up.
// `changelevel` answers and then drops the connection; the old client returned
// ("", EOF) and the operator saw a failure for a map change that worked.
func TestExecKeepsOutputWhenTheServerHangsUp(t *testing.T) {
	s := newRawServer(t, func(conn net.Conn) {
		p, err := readFrame(conn)
		if err != nil {
			return
		}
		writeFrame(conn, p.id, typeServerDataResponseValue, []byte("Changing level to de_nuke"))
		_ = conn.Close()
	})
	c := dialRaw(t, s.addr())
	out, err := c.Exec("changelevel de_nuke")
	if out != "Changing level to de_nuke" {
		t.Fatalf("out = %q (err %v) — collected output was discarded", out, err)
	}
}

// A single trailing NUL must not cost the body its last byte.
func TestReadPacketKeepsTheLastByteWithOneTerminator(t *testing.T) {
	s := newRawServer(t, func(conn net.Conn) {
		p, err := readFrame(conn)
		if err != nil {
			return
		}
		body := []byte("hostname: x")
		// size counts id + type + body + a single NUL.
		writeRawFrame(conn, uint32(4+4+len(body)+1), p.id, typeServerDataResponseValue, append(body, 0))
	})
	c := dialRaw(t, s.addr())
	out, err := c.Exec("status")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out != "hostname: x" {
		t.Fatalf("out = %q, want the full body", out)
	}
}

// A frame the client cannot parse desynchronizes the stream forever. It must
// retire the connection rather than reading the middle of a body as a length and
// failing every later command with a nonsense byte count.
func TestBadFrameRetiresTheConnection(t *testing.T) {
	s := newRawServer(t, func(conn net.Conn) {
		if _, err := readFrame(conn); err != nil {
			return
		}
		// A size below the protocol minimum, then plausible ASCII.
		writeRawFrame(conn, 4, 0, 0, nil)
		_, _ = conn.Write([]byte("AAAAAAAAAAAAAAAAAAAA"))
		time.Sleep(time.Second)
	})
	c := dialRaw(t, s.addr())
	if _, err := c.Exec("status"); !errors.Is(err, ErrBadPacket) {
		t.Fatalf("first exec err = %v, want ErrBadPacket", err)
	}
	_, err := c.Exec("status")
	if !errors.Is(err, ErrBadPacket) {
		t.Fatalf("second exec err = %v, want the same framing error, not a fresh misread", err)
	}
	if strings.Contains(err.Error(), "1094795585") {
		t.Fatalf("err = %v — the client read ASCII as a length", err)
	}
}

// A body of exactly the protocol maximum is legal and must be accepted.
func TestReadPacketAcceptsAFullSizeBody(t *testing.T) {
	body := strings.Repeat("z", maxBodySize)
	s := newRawServer(t, func(conn net.Conn) {
		p, err := readFrame(conn)
		if err != nil {
			return
		}
		writeFrame(conn, p.id, typeServerDataResponseValue, []byte(body))
		writeFrame(conn, p.id, typeServerDataResponseValue, nil)
	})
	c := dialRaw(t, s.addr())
	out, err := c.Exec("status")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(out) != maxBodySize {
		t.Fatalf("got %d bytes, want %d", len(out), maxBodySize)
	}
}

// A server that sends several empty RESPONSE_VALUE frames before the
// AUTH_RESPONSE (observed on Source builds and RCON proxies) authenticated
// correctly but was reported as "malformed packet".
func TestAuthToleratesExtraEmptyFramesFirst(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		p, err := readFrame(conn)
		if err != nil {
			return
		}
		for i := 0; i < 4; i++ {
			writeFrame(conn, p.id, typeServerDataResponseValue, nil)
		}
		writeFrame(conn, p.id, typeServerDataAuthResponse, nil)
		time.Sleep(200 * time.Millisecond)
	}()
	c, err := Dial(ln.Addr().String(), "pw", 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v — a correct password was rejected", err)
	}
	_ = c.Close()
}

// CS2 refuses a wrong password by closing the connection. That must be reported
// as an authentication failure, not as a bare EOF: the diagnosis mapped the
// unrecognised error to "give the server a minute to load the map", so the most
// common RCON misconfiguration was diagnosed as impatience.
func TestAuthFailureOnCloseIsRecognised(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = readFrame(conn)
		_ = conn.Close() // hang up without answering
	}()
	_, err = Dial(ln.Addr().String(), "wrong", 2*time.Second)
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("err = %v, want ErrAuthFailed", err)
	}
}

// Fire treats "no answer" as success, so it must first notice a socket the
// server has already closed. Otherwise a buffered write succeeds and the panel
// reports "map change started" for a command that never arrived.
func TestFireFailsOnAClosedConnection(t *testing.T) {
	closed := make(chan struct{})
	s := newRawServer(t, func(conn net.Conn) {
		_ = conn.Close()
		close(closed)
		time.Sleep(500 * time.Millisecond)
	})
	c := dialRaw(t, s.addr())
	<-closed
	time.Sleep(50 * time.Millisecond) // let the FIN arrive
	if _, err := c.Fire("changelevel de_nuke", 200*time.Millisecond); err == nil {
		t.Fatal("Fire reported success on a connection the server had closed")
	}
}

// Fire on a healthy connection still succeeds when the server says nothing:
// that is the whole point of Fire, and the liveness check must not break it.
func TestFireSucceedsWhenTheServerStaysSilent(t *testing.T) {
	s := newRawServer(t, func(conn net.Conn) {
		_, _ = readFrame(conn)
		time.Sleep(700 * time.Millisecond) // never answers
	})
	c := dialRaw(t, s.addr())
	if _, err := c.Fire("changelevel de_nuke", 100*time.Millisecond); err != nil {
		t.Fatalf("Fire: %v", err)
	}
}

// A cancelled request must stop the read. The status page gives up after 15 s
// while ExecTimeout is 20 s, so without a context the agent kept waiting on a
// hibernating server long after nobody was listening.
func TestExecContextIsBoundedByTheContext(t *testing.T) {
	s := newRawServer(t, func(conn net.Conn) {
		_, _ = readFrame(conn)
		time.Sleep(3 * time.Second) // never answers
	})
	c := dialRaw(t, s.addr())
	c.ExecTimeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.ExecContext(ctx, "status"); err == nil {
		t.Fatal("want an error when the context expires")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ExecContext took %v; the context deadline was ignored", elapsed)
	}
}

// The happy path with a server that does not implement the end-marker trick:
// the response must still come back complete, via the idle window.
func TestExecWorksWithoutTheEndMarker(t *testing.T) {
	s := newFakeServer(t, "pw")
	s.noMarker = true
	s.on("status", "hostname: quiet server")
	c, err := Dial(s.addr(), "pw", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.ExecReadIdle = 100 * time.Millisecond
	out, err := c.Exec("status")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out != "hostname: quiet server" {
		t.Fatalf("out = %q", out)
	}
}

// Two commands on one connection must not bleed into each other: the end-marker
// exchange has to be fully consumed before the next command reads.
func TestExecTwiceOnOneConnection(t *testing.T) {
	s := newFakeServer(t, "pw")
	s.on("status", "first answer")
	s.on("stats", "second answer")
	c, err := Dial(s.addr(), "pw", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.ExecReadIdle = 100 * time.Millisecond
	if out, err := c.Exec("status"); err != nil || out != "first answer" {
		t.Fatalf("first = %q err=%v", out, err)
	}
	if out, err := c.Exec("stats"); err != nil || out != "second answer" {
		t.Fatalf("second = %q err=%v", out, err)
	}
}
