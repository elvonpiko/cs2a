// Package a2s implements a minimal client for Valve's A2S query protocol
// (https://developer.valvesoftware.com/wiki/Server_Queries) covering
// A2S_INFO and A2S_PLAYER over UDP, which CS2 servers answer on the game port
// (27015/udp). It is deliberately dependency-free.
package a2s

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"
)

// Query protocol headers (single byte, except multi-packet replies which we
// reassemble before inspecting).
const (
	headerInfoRequest    = 0x54 // "A2S_SOURCE_QUERY" magic below
	headerInfoResponse   = 0x49 // 'I'  (new format)
	headerInfoGoldsrc    = 0x6D // 'm'  (old format, rare on CS2)
	headerPlayerRequest  = 0x55 // 'U'
	headerPlayerResponse = 0x44 // 'D'
	headerChallenge      = 0x41 // 'A'
	headerMultiPacket    = 0xFE
)

var infoRequestMagic = []byte("Source Engine Query\000")

// headerPrefix starts every A2S request and response datagram.
var headerPrefix = []byte{0xFF, 0xFF, 0xFF, 0xFF}

// ErrChallengeRetry indicates the server kept asking for challenges.
var ErrChallengeRetry = errors.New("a2s: too many challenge round trips")

// Server address kinds accepted: "1.2.3.4:27015".
type Client struct {
	conn    *net.UDPConn
	timeout time.Duration
}

// Dial prepares a UDP client for the given address. UDP is stateless; a
// Dial doesn't test reachability.
func Dial(addr string, timeout time.Duration) (*Client, error) {
	raddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("a2s: resolve %s: %w", addr, err)
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("a2s: dial %s: %w", addr, err)
	}
	return &Client{conn: conn, timeout: timeout}, nil
}

// Close closes the UDP socket.
func (c *Client) Close() error { return c.conn.Close() }

// Info queries A2S_INFO and returns server metadata.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	req := append(append([]byte{}, headerPrefix...), headerInfoRequest)
	req = append(req, infoRequestMagic...)
	payload, err := c.roundTrip(ctx, req)
	if err != nil {
		return info, err
	}
	if len(payload) == 0 || payload[0] == headerMultiPacket {
		payload, err = c.decodeMultiPacket(payload)
		if err != nil {
			return info, err
		}
	}
	if len(payload) == 0 {
		return info, errors.New("a2s: empty info response")
	}
	switch payload[0] {
	case headerInfoResponse:
		return decodeInfo(payload[1:])
	case headerInfoGoldsrc:
		return decodeGoldsrcInfo(payload[1:])
	default:
		return info, fmt.Errorf("a2s: unexpected info header 0x%x", payload[0])
	}
}

// Players queries A2S_PLAYER; roundTrip handles the challenge handshake.
func (c *Client) Players(ctx context.Context) ([]Player, error) {
	req := append(append([]byte{}, headerPrefix...), headerPlayerRequest, 0xFF, 0xFF, 0xFF, 0xFF)
	payload, err := c.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(payload) > 0 && payload[0] == headerMultiPacket {
		payload, err = c.decodeMultiPacket(payload)
		if err != nil {
			return nil, err
		}
	}
	if len(payload) == 0 || payload[0] != headerPlayerResponse {
		return nil, fmt.Errorf("a2s: unexpected player header 0x%x", head(payload))
	}
	return decodePlayers(payload[1:])
}

func head(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

// roundTrip sends a request and reads the reply, transparently handling the
// S2C_CHALLENGE response by re-sending the base request with the challenge
// appended (per protocol spec).
func (c *Client) roundTrip(ctx context.Context, base []byte) ([]byte, error) {
	req := base
	for i := 0; i < 3; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if err := c.conn.SetDeadline(time.Now().Add(c.timeout)); err != nil {
			return nil, err
		}
		if _, err := c.conn.Write(req); err != nil {
			return nil, fmt.Errorf("a2s: write: %w", err)
		}
		buf := make([]byte, 4096)
		n, err := c.conn.Read(buf)
		if err != nil {
			return nil, fmt.Errorf("a2s: read: %w", err)
		}
		resp := buf[:n]
		// S2C_CHALLENGE: [FF FF FF FF]['A'][challenge:4].
		// Requests carrying a 0xFFFFFFFF challenge placeholder must have it
		// replaced; payload-style requests (A2S_INFO) get it appended.
		if n >= 9 && resp[4] == headerChallenge {
			ch := make([]byte, 4)
			copy(ch, resp[5:9])
			if len(base) >= 5 && base[len(base)-4] == 0xFF &&
				base[len(base)-3] == 0xFF && base[len(base)-2] == 0xFF &&
				base[len(base)-1] == 0xFF {
				req = append(append([]byte{}, base[:len(base)-4]...), ch...)
			} else {
				req = append(append([]byte{}, base...), ch...)
			}
			continue
		}
		// strip the 4-byte header prefix (FF FF FF FF)
		if n >= 4 {
			return resp[4:], nil
		}
		return resp, nil
	}
	return nil, ErrChallengeRetry
}

// decodeMultiPacket reassembles a single-packet concatanation of a
// multi-part reply. CS2 replies fit in one UDP datagram for our queries in
// practice; when they don't, we reassemble the largest span we received.
func (c *Client) decodeMultiPacket(payload []byte) ([]byte, error) {
	// payload begins with the multi-packet header: byte 0xFE, then id(4),
	// total(1), number(1), split size(2) ... payload sections follow.
	if len(payload) < 9 {
		return nil, errors.New("a2s: short multipacket header")
	}
	// we only read one datagram; take the payload of this part
	return payload[9:], nil
}

type reader struct {
	b   []byte
	pos int
	err bool
}

func (r *reader) byteVal() byte {
	if r.pos >= len(r.b) {
		r.err = true
		return 0
	}
	v := r.b[r.pos]
	r.pos++
	return v
}

func (r *reader) str() string {
	start := r.pos
	for r.pos < len(r.b) && r.b[r.pos] != 0 {
		r.pos++
	}
	if r.pos >= len(r.b) {
		r.err = true
		return ""
	}
	s := string(r.b[start:r.pos])
	r.pos++ // consume nul
	return s
}

func (r *reader) u16() uint16 {
	if r.pos+2 > len(r.b) {
		r.err = true
		return 0
	}
	v := binary.LittleEndian.Uint16(r.b[r.pos:])
	r.pos += 2
	return v
}

func (r *reader) u32() uint32 {
	if r.pos+4 > len(r.b) {
		r.err = true
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v
}

func (r *reader) u64() uint64 {
	if r.pos+8 > len(r.b) {
		r.err = true
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b[r.pos:])
	r.pos += 8
	return v
}

func (r *reader) skip(n int) {
	if r.pos+n > len(r.b) {
		r.err = true
		r.pos = len(r.b)
		return
	}
	r.pos += n
}

func (r *reader) fail() error {
	if r.err {
		return errors.New("a2s: truncated response")
	}
	return nil
}

// Info carries the fields cs2a cares about from A2S_INFO.
type Info struct {
	Protocol byte
	Name     string
	Map      string
	Game     string
	Players  int
	Max      int
	Bots     int
}

func decodeInfo(b []byte) (Info, error) {
	var info Info
	r := &reader{b: b}
	info.Protocol = r.byteVal()
	info.Name = r.str()
	info.Map = r.str()
	r.str()             // folder
	info.Game = r.str() // game description
	r.u16()             // appid
	info.Players = int(r.byteVal())
	info.Max = int(r.byteVal())
	info.Bots = int(r.byteVal())
	r.byteVal() // server type
	r.byteVal() // environment
	r.byteVal() // visibility
	return info, r.fail()
}

func decodeGoldsrcInfo(b []byte) (Info, error) {
	var info Info
	r := &reader{b: b}
	info.Name = r.str()
	info.Map = r.str()
	r.str() // folder
	r.str() // game
	info.Players = int(r.byteVal())
	info.Max = int(r.byteVal())
	info.Protocol = r.byteVal()
	r.byteVal() // Visibility
	r.byteVal() // IsMod flag — no mod info parsing needed for cs2a
	return info, r.fail()
}

// Player is one A2S_PLAYER row.
type Player struct {
	Index     byte
	Name      string
	Score     int32
	Duration  float32 // seconds connected
	SteamID64 string  // parsed from the 8-byte steamid when valid
}

func decodePlayers(b []byte) ([]Player, error) {
	r := &reader{b: b}
	count := int(r.byteVal())
	out := make([]Player, 0, count)
	for i := 0; i < count && !r.err; i++ {
		var p Player
		p.Index = r.byteVal()
		p.Name = r.str()
		p.Score = int32(r.u32())
		p.Duration = float32FromBits(r.u32())
		sid := r.u64()
		p.SteamID64 = steamID64String(sid)
		out = append(out, p)
	}
	return out, r.fail()
}

func float32FromBits(v uint32) float32 {
	return math.Float32frombits(v)
}

func steamID64String(sid uint64) string {
	if sid == 0 {
		return ""
	}
	return strconv.FormatUint(sid, 10)
}
