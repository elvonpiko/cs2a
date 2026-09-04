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

// maxPacketSize is the protocol cap: body may hold at most 4096 bytes
// (4113 = 4096 body + 4 id + 4 type + 1 nul... spec allows slightly more with
// two trailing nuls; we accept up to this size defensively).
const maxPacketSize = 4113

// Errors returned by the client.
var (
	ErrAuthFailed = errors.New("rcon: authentication rejected")
	ErrBadPacket  = errors.New("rcon: malformed packet")
	ErrClosed     = errors.New("rcon: connection closed")
)

// Client is a single RCON connection. It is NOT safe for concurrent use;
// higher layers should serialize commands (a dedicated server effectively
// answers them sequentially anyway).
type Client struct {
	conn net.Conn

	// nextID is the packet id for the next outgoing request.
	nextID int32

	// ExecReadIdle is how long Exec waits for a *further* packet after the
	// first one before assuming the response is complete. Servers may answer
	// long commands with several packets followed by an empty terminator.
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
		nextID:       1,
		ExecReadIdle: 350 * time.Millisecond,
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

func (c *Client) auth(password string) error {
	reqID := c.nextID
	c.nextID++
	if err := c.writePacket(reqID, typeServerDataAuth, password); err != nil {
		return err
	}
	// The server answers auth with an empty RESPONSE_VALUE, then the
	// AUTH_RESPONSE whose id is -1 on rejection.
	for i := 0; i < 2; i++ {
		p, err := c.readPacket()
		if err != nil {
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
	return ErrBadPacket
}

// Exec runs a server console command and returns its (multi-packet) output.
func (c *Client) Exec(cmd string) (string, error) {
	reqID := c.nextID
	c.nextID++
	if err := c.writePacket(reqID, typeServerDataExecCommand, cmd); err != nil {
		return "", err
	}

	var out []byte
	packets := 0
	for packets < 16 {
		c.setDeadline(c.ExecReadIdle)
		p, err := c.readPacket()
		if err != nil {
			// An idle timeout after at least one packet means we have the
			// whole response — that is the normal exit path.
			var ne net.Error
			if packets > 0 && errors.As(err, &ne) && ne.Timeout() {
				break
			}
			return "", err
		}
		packets++
		if p.typ != typeServerDataResponseValue || p.id != reqID {
			continue
		}
		if len(p.body) == 0 {
			break // explicit empty terminator
		}
		out = append(out, p.body...)
	}
	return string(out), nil
}

// Close closes the underlying connection.
func (c *Client) Close() error { return c.conn.Close() }

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
	var lenBuf [4]byte
	if _, err := io.ReadFull(c.conn, lenBuf[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return packet{}, ErrClosed
		}
		return packet{}, err
	}
	size := binary.LittleEndian.Uint32(lenBuf[:])
	if size < 10 || size > maxPacketSize {
		return packet{}, fmt.Errorf("%w: bad size %d", ErrBadPacket, size)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(c.conn, payload); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return packet{}, ErrClosed
		}
		return packet{}, err
	}
	id := int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ := int32(binary.LittleEndian.Uint32(payload[4:8]))
	// body runs up to the first of the two trailing nul bytes
	body := payload[8 : size-2]
	return packet{id: id, typ: typ, body: body}, nil
}
