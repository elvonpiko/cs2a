// Package rcon implements a minimal client for Valve's Source RCON protocol
// (https://developer.valvesoftware.com/wiki/Source_RCON_Protocol), which CS2's
// dedicated server still speaks on the game port when launched with -usercon.
//
// Frame layout (all integers little-endian):
//
//	[int32 size][int32 id][int32 type][body bytes][0x00 0x00]
//
// size counts everything after the size field itself.
package rcon

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Server -> client / client -> server packet types.
const (
	typeServerDataResponseValue = 0
	typeServerDataAuthResponse  = 2
	typeServerDataExecCommand   = 2
	typeServerDataAuth          = 3
)

// maxBodySize is the largest body SRCDS puts in one packet; a longer response is
// split across several packets. A body of exactly this length therefore means
// "more is coming" — the client relies on that rather than on a timing guess.
const maxBodySize = 4096

// maxFrameSize bounds what the client is willing to allocate for one frame. It
// is far above the protocol's own limit on purpose: rejecting a slightly
// oversized frame used to leave the unread body in the socket, and every later
// command then read the middle of that body as a length and failed with a
// nonsense byte count. The cap exists only so a hostile length cannot exhaust
// memory; anything over it is a framing error that closes the connection.
const maxFrameSize = 64 * 1024

// maxResponseSize bounds one reassembled response.
const maxResponseSize = 4 * 1024 * 1024

// minFrameSize is id(4) + type(4) + two terminators.
const minFrameSize = 10

// Errors returned by the client.
var (
	ErrAuthFailed = errors.New("rcon: authentication rejected")
	ErrBadPacket  = errors.New("rcon: malformed packet")
	ErrClosed     = errors.New("rcon: connection closed")
	// ErrTruncated reports that the server's answer was cut short. The output
	// collected so far is returned with it: half a `status` is worth showing,
	// but it must never be passed off as the whole answer.
	ErrTruncated = errors.New("rcon: response truncated")
)

// Client is a single RCON connection. It is NOT safe for concurrent use;
// higher layers should serialize commands (a dedicated server effectively
// answers them sequentially anyway).
type Client struct {
	conn net.Conn
	br   *bufio.Reader

	// nextID is the packet id for the next outgoing request.
	nextID int32

	// broken records the framing error that desynchronized this connection.
	// Once set, every further call fails with it instead of misreading the
	// stream: a client that has lost frame alignment can never recover.
	broken error

	// ExecTimeout bounds how long Exec waits for the *first* response packet.
	// It must be generous: commands such as `changelevel` block the server
	// while it loads the next map, so the reply can take seconds.
	ExecTimeout time.Duration

	// ExecReadIdle is how long Exec waits, after a response that looks
	// complete, for the end marker or a straggling packet. It is short because
	// this window is paid on every command; the mandatory wait for a split
	// response's continuation is separate and longer.
	ExecReadIdle time.Duration
}

// Dial connects to the RCON endpoint (e.g. "127.0.0.1:27015") and performs the
// AUTH handshake. timeout bounds both the TCP dial and each protocol read.
func Dial(addr, password string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("rcon: dial %s: %w", addr, err)
	}
	c := &Client{
		conn:         conn,
		br:           bufio.NewReaderSize(conn, maxBodySize+minFrameSize+4),
		nextID:       1,
		ExecTimeout:  20 * time.Second,
		ExecReadIdle: 250 * time.Millisecond,
	}
	c.setDeadline(timeout)
	if err := c.auth(password); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) setDeadline(d time.Duration) {
	_ = c.conn.SetDeadline(time.Now().Add(d))
}

// authMaxPackets bounds the handshake. The spec describes one empty
// RESPONSE_VALUE followed by the AUTH_RESPONSE, but Source builds and RCON
// proxies have been seen sending several empty frames first; stopping after two
// reported "malformed packet" for a password that was in fact correct.
const authMaxPackets = 8

func (c *Client) auth(password string) error {
	reqID := c.nextID
	c.nextID++
	if err := c.writePacket(reqID, typeServerDataAuth, password); err != nil {
		return err
	}
	for i := 0; i < authMaxPackets; i++ {
		p, err := c.readPacket()
		if err != nil {
			// CS2 answers a wrong password by closing the connection rather
			// than sending a rejection, so a bare EOF here IS the rejection.
			// Reporting it as a network fault sent the operator looking for a
			// firewall problem while the diagnosis said "wait for the map to
			// load".
			if isPeerClosed(err) {
				return fmt.Errorf("%w: the server closed the connection without accepting the password", ErrAuthFailed)
			}
			return err
		}
		if p.typ != typeServerDataAuthResponse {
			continue
		}
		if p.id == -1 {
			return ErrAuthFailed
		}
		return nil
	}
	return fmt.Errorf("%w: no auth response after %d packets", ErrBadPacket, authMaxPackets)
}

// Exec runs a server console command and returns its (multi-packet) output.
func (c *Client) Exec(cmd string) (string, error) {
	return c.ExecContext(context.Background(), cmd)
}

// ExecContext is Exec bounded by ctx as well as by ExecTimeout. A status page
// that gave up after 15 s used to leave the agent waiting the full 20 s on a
// hibernating server, with no way for the cancelled request to stop it.
func (c *Client) ExecContext(ctx context.Context, cmd string) (string, error) {
	if c.broken != nil {
		return "", c.broken
	}
	cmdID := c.nextID
	endID := c.nextID + 1
	c.nextID += 2
	if err := c.writePacket(cmdID, typeServerDataExecCommand, cmd); err != nil {
		return "", err
	}
	// The end marker: SRCDS mirrors back an (otherwise invalid) empty
	// RESPONSE_VALUE request, and because it answers in order, that mirror
	// proves the real response is complete. Without it a split response can only
	// be guessed at from timing, and a slow server made `status` come back half
	// parsed with err == nil. A server that ignores the marker simply falls back
	// to the idle window below, so sending it can only help.
	_ = c.writePacket(endID, typeServerDataResponseValue, "")

	deadline := time.Now().Add(c.execTimeout())
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	var out []byte
	responses := 0
	// splitPending means the last packet was full-size, so the server has
	// definitely not finished: its continuation is mandatory and a missing one
	// is data loss, not a complete answer.
	splitPending := false
	for {
		if err := ctx.Err(); err != nil {
			return string(out), err
		}
		if len(out) > maxResponseSize {
			// A runaway response (a misbehaving server, or a modded build's
			// cvarlist) must not grow without bound.
			return string(out), fmt.Errorf("%w: response exceeds %d bytes", ErrTruncated, len(out))
		}
		until := deadline
		switch {
		case splitPending:
			// A continuation is outstanding. Packets of a split response follow
			// each other immediately, so this wait is short — the generous
			// ExecTimeout budget exists for the server's *first* answer.
			if w := time.Now().Add(c.splitWait()); w.Before(until) {
				until = w
			}
		case responses > 0:
			// The answer looks complete; a short window catches the end marker
			// (or a straggling packet) before we call it done.
			if idle := time.Now().Add(c.execReadIdle()); idle.Before(until) {
				until = idle
			}
		}
		_ = c.conn.SetDeadline(until)
		p, err := c.readPacket()
		if err != nil {
			var ne net.Error
			isTimeout := errors.As(err, &ne) && ne.Timeout()
			switch {
			case isTimeout && responses > 0 && !splitPending:
				// A complete-looking response followed by silence. With the end
				// marker outstanding this means the server does not implement
				// the trick, which is the documented fallback.
				return string(out), nil
			case isTimeout && splitPending:
				// The server stopped mid-response. Returning what arrived as a
				// success handed the caller half a `status` to parse.
				return string(out), fmt.Errorf("%w: the server stopped answering after %d bytes", ErrTruncated, len(out))
			case isPeerClosed(err) && responses > 0:
				return string(out), fmt.Errorf("%w: the server closed the connection after %d bytes", ErrTruncated, len(out))
			default:
				return string(out), err
			}
		}
		if p.id == endID {
			// The mirrored marker: everything meaningful has arrived.
			c.drainMarker(endID)
			return string(out), nil
		}
		if p.typ != typeServerDataResponseValue || p.id != cmdID {
			// Not our answer: an out-of-band frame must not count as one, or a
			// server answering with a mismatched id looked like an empty but
			// successful response.
			continue
		}
		responses++
		if len(p.body) == 0 && len(out) > 0 {
			return string(out), nil // explicit empty terminator
		}
		if len(p.body) == 0 {
			// An empty first frame is ambiguous: it is the terminator for a
			// command with no output, but some servers (and RCON proxies) send
			// an empty ack before the real answer. Treating it as the end
			// returned "" for a command that did answer, so wait for either the
			// end marker or the idle window instead.
			continue
		}
		out = append(out, p.body...)
		splitPending = len(p.body) == maxBodySize
	}
}

// drainMarker consumes the second half of the end-marker exchange (SRCDS sends
// the mirrored empty packet followed by one carrying a fixed 8-byte body) so the
// next command on this connection does not read it as its own answer.
func (c *Client) drainMarker(endID int32) {
	for i := 0; i < 2; i++ {
		_ = c.conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		p, err := c.readPacket()
		if err != nil || p.id != endID {
			return
		}
	}
}

// Fire sends a command without requiring the server to answer. Commands such
// as `changelevel` stall the game server while it loads, and CS2 may close the
// RCON connection instead of replying — for those, a missing reply is success,
// not an error. Any output that does arrive within grace is returned.
//
// Because "no answer" is treated as success, Fire first checks that the peer is
// still there: writing into a socket the server has already closed succeeds
// (the data is merely buffered), so a command that never reached the server used
// to be reported as "map change started".
func (c *Client) Fire(cmd string, grace time.Duration) (string, error) {
	if c.broken != nil {
		return "", c.broken
	}
	if err := c.peerGone(); err != nil {
		return "", err
	}
	reqID := c.nextID
	c.nextID++
	if err := c.writePacket(reqID, typeServerDataExecCommand, cmd); err != nil {
		return "", err
	}
	if grace <= 0 {
		return "", nil
	}
	c.setDeadline(grace)
	p, err := c.readPacket()
	if err != nil {
		return "", nil // no (or truncated) reply is expected for these commands
	}
	if p.typ != typeServerDataResponseValue || p.id != reqID {
		return "", nil
	}
	return string(p.body), nil
}

// peerGone reports a connection the server has already closed. A queued FIN or
// RST is delivered to a read immediately, so a very short deadline is enough to
// tell "hung up" from "idle but healthy"; the wait is only paid when the socket
// really is idle. Peeking rather than reading leaves any pending frame intact.
func (c *Client) peerGone() error {
	_ = c.conn.SetReadDeadline(time.Now().Add(peerCheckWait))
	_, err := c.br.Peek(1)
	// Restore an open-ended deadline for the caller's own read.
	_ = c.conn.SetReadDeadline(time.Time{})
	if err == nil {
		return nil // data waiting: the peer is very much alive
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return nil // nothing queued: healthy idle socket
	}
	if isPeerClosed(err) {
		return fmt.Errorf("%w: the game server closed the RCON connection", ErrClosed)
	}
	return err
}

// peerCheckWait is how long the liveness peek waits before concluding the socket
// is merely idle.
const peerCheckWait = 25 * time.Millisecond

// Close closes the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }

// execTimeout is ExecTimeout with a safe default for zero-valued clients.
func (c *Client) execTimeout() time.Duration {
	if c.ExecTimeout > 0 {
		return c.ExecTimeout
	}
	return 20 * time.Second
}

func (c *Client) execReadIdle() time.Duration {
	if c.ExecReadIdle > 0 {
		return c.ExecReadIdle
	}
	return 250 * time.Millisecond
}

// splitWait is how long a mandatory continuation packet may take to arrive.
// Packets of one split response are written back-to-back by the server, so this
// only has to survive scheduling and network jitter.
func (c *Client) splitWait() time.Duration {
	return 10 * c.execReadIdle()
}

type packet struct {
	id   int32
	typ  int32
	body []byte
}

func (c *Client) writePacket(id, typ int32, body string) error {
	size := 4 + 4 + len(body) + 2
	buf := make([]byte, 4+size)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(size))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(typ))
	copy(buf[12:], body)
	// trailing two nul bytes already zero-valued
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write(buf); err != nil {
		return fmt.Errorf("rcon: write: %w", err)
	}
	return nil
}

func (c *Client) readPacket() (packet, error) {
	if c.broken != nil {
		return packet{}, c.broken
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(c.br, lenBuf[:]); err != nil {
		return packet{}, c.readError(err)
	}
	size := binary.LittleEndian.Uint32(lenBuf[:])
	if size < minFrameSize || size > maxFrameSize {
		// The stream no longer starts where a frame starts. There is no way to
		// resynchronize, so the connection is retired instead of returning
		// nonsense for every later command.
		return packet{}, c.breakConn(fmt.Errorf("%w: bad size %d", ErrBadPacket, size))
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return packet{}, c.readError(err)
	}
	id := int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ := int32(binary.LittleEndian.Uint32(payload[4:8]))
	// The body is followed by its own nul terminator plus the empty-string nul.
	// Trimming a fixed two bytes lost the body's last character on servers that
	// send only one, so trim what is actually there (at most two).
	body := payload[8:]
	for i := 0; i < 2 && len(body) > 0 && body[len(body)-1] == 0; i++ {
		body = body[:len(body)-1]
	}
	return packet{id: id, typ: typ, body: body}, nil
}

// readError normalizes a read failure. A short read in the middle of a frame is
// unrecoverable, so it also retires the connection.
func (c *Client) readError(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return c.breakConn(fmt.Errorf("%w: incomplete packet", ErrClosed))
	}
	if errors.Is(err, io.EOF) {
		return c.breakConn(ErrClosed)
	}
	return err // timeouts stay retryable
}

// breakConn records a fatal framing error and closes the socket.
func (c *Client) breakConn(err error) error {
	if c.broken == nil {
		c.broken = err
	}
	_ = c.conn.Close()
	return c.broken
}

// isPeerClosed reports the errors that mean "the server hung up".
func isPeerClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, ErrClosed) || errors.Is(err, net.ErrClosed)
}
