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
	"sort"
	"time"
)

// Query protocol headers (single byte, after the 4-byte datagram header).
const (
	headerInfoRequest    = 0x54 // "A2S_SOURCE_QUERY" magic below
	headerInfoResponse   = 0x49 // 'I'  (Source format)
	headerInfoGoldsrc    = 0x6D // 'm'  (GoldSrc format, rare on CS2)
	headerPlayerRequest  = 0x55 // 'U'
	headerPlayerResponse = 0x44 // 'D'
	headerChallenge      = 0x41 // 'A'
)

// Datagram headers. The first four bytes of every A2S datagram are an int32:
// -1 for a whole reply, -2 for one fragment of a split reply.
var (
	singlePacketHeader = []byte{0xFF, 0xFF, 0xFF, 0xFF}
	splitPacketHeader  = []byte{0xFE, 0xFF, 0xFF, 0xFF}
)

var infoRequestMagic = []byte("Source Engine Query\000")

// headerPrefix starts every A2S request datagram.
var headerPrefix = singlePacketHeader

// Errors returned by the client.
var (
	ErrChallengeRetry = errors.New("a2s: too many challenge round trips")
	// ErrIncomplete reports a split reply whose fragments did not all arrive.
	// It must be an error: silently decoding the fragments that did show up
	// produced a plausible-looking Info with a truncated map name or a player
	// list missing its tail.
	ErrIncomplete = errors.New("a2s: incomplete split reply")
)

// maxDatagram is the largest UDP payload that can arrive. The old 4096-byte
// buffer silently discarded the rest of any larger datagram — a full 64-slot
// A2S_PLAYER reply runs past 4 KB — and the decode then failed with a confusing
// "truncated response" or, worse, succeeded on half the list.
const maxDatagram = 65535

// maxFragments bounds reassembly. Real replies use a handful of fragments; the
// cap stops a hostile or looping server from making the client wait forever.
const maxFragments = 32

// Client queries one server address, e.g. "1.2.3.4:27015".
type Client struct {
	conn    *net.UDPConn
	timeout time.Duration
}

// Dial prepares a UDP client for the given address. UDP is stateless, so Dial
// does not test reachability.
func Dial(addr string, timeout time.Duration) (*Client, error) {
	raddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("a2s: resolve %s: %w", addr, err)
	}
	conn, err := net.DialUDP("udp4", nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("a2s: dial %s: %w", addr, err)
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Client{conn: conn, timeout: timeout}, nil
}

// Close closes the UDP socket.
func (c *Client) Close() error { return c.conn.Close() }

// Info queries A2S_INFO and returns server metadata.
func (c *Client) Info(ctx context.Context) (Info, error) {
	req := append(append([]byte{}, headerPrefix...), headerInfoRequest)
	req = append(req, infoRequestMagic...)
	payload, err := c.roundTrip(ctx, req)
	if err != nil {
		return Info{}, err
	}
	if len(payload) == 0 {
		return Info{}, errors.New("a2s: empty info response")
	}
	switch payload[0] {
	case headerInfoResponse:
		return decodeInfo(payload[1:])
	case headerInfoGoldsrc:
		return decodeGoldsrcInfo(payload[1:])
	default:
		return Info{}, fmt.Errorf("a2s: unexpected info header 0x%x", payload[0])
	}
}

// Players queries A2S_PLAYER; roundTrip handles the challenge handshake.
func (c *Client) Players(ctx context.Context) ([]Player, error) {
	req := append(append([]byte{}, headerPrefix...), headerPlayerRequest, 0xFF, 0xFF, 0xFF, 0xFF)
	payload, err := c.roundTrip(ctx, req)
	if err != nil {
		return nil, err
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

// roundTrip sends a request, handles the S2C_CHALLENGE handshake, reassembles a
// split reply, and returns the reply payload with its 4-byte datagram header
// removed.
func (c *Client) roundTrip(ctx context.Context, base []byte) ([]byte, error) {
	req := base
	for i := 0; i < 3; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.setDeadline(ctx); err != nil {
			return nil, err
		}
		if _, err := c.conn.Write(req); err != nil {
			return nil, fmt.Errorf("a2s: write: %w", err)
		}
		resp, err := c.readDatagram(ctx)
		if err != nil {
			return nil, err
		}
		// S2C_CHALLENGE: [FF FF FF FF]['A'][challenge:4].
		if len(resp) >= 9 && resp[4] == headerChallenge {
			req = withChallenge(base, resp[5:9])
			continue
		}
		if bytesHavePrefix(resp, splitPacketHeader) {
			return c.reassemble(ctx, resp)
		}
		if len(resp) >= 4 {
			return resp[4:], nil
		}
		return resp, nil
	}
	return nil, ErrChallengeRetry
}

// withChallenge folds a challenge into the request: a 0xFFFFFFFF placeholder is
// replaced, otherwise the challenge is appended (A2S_INFO).
func withChallenge(base, challenge []byte) []byte {
	ch := append([]byte{}, challenge...)
	if n := len(base); n >= 5 && base[n-4] == 0xFF && base[n-3] == 0xFF &&
		base[n-2] == 0xFF && base[n-1] == 0xFF {
		return append(append([]byte{}, base[:n-4]...), ch...)
	}
	return append(append([]byte{}, base...), ch...)
}

func bytesHavePrefix(b, prefix []byte) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range prefix {
		if b[i] != prefix[i] {
			return false
		}
	}
	return true
}

// setDeadline bounds the next socket operation by both the per-read timeout and
// the caller's context. Without the context bound a slow server could hold a
// query for timeout seconds *after* the caller had already given up, which is
// how a 15 s page request ended up waiting on a 3 s-timeout client for longer.
func (c *Client) setDeadline(ctx context.Context) error {
	until := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(until) {
		until = d
	}
	return c.conn.SetDeadline(until)
}

func (c *Client) readDatagram(ctx context.Context) ([]byte, error) {
	if err := c.setDeadline(ctx); err != nil {
		return nil, err
	}
	buf := make([]byte, maxDatagram)
	n, err := c.conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("a2s: read: %w", err)
	}
	return buf[:n], nil
}

// fragment is one piece of a split reply.
type fragment struct {
	number byte
	body   []byte
}

// reassemble collects the remaining fragments of a split reply and concatenates
// them in order.
//
// Split-reply header (Source):
//
//	int32 -2 | int32 id | byte total | byte number | int16 split size | payload
//
// The old code read a single datagram, threw away the header and returned that
// one fragment's payload as the whole reply — so any reply over ~1400 bytes
// (a long hostname, a populated player list) decoded as garbage or reported a
// truncation the caller could not explain.
func (c *Client) reassemble(ctx context.Context, first []byte) ([]byte, error) {
	id, total, frag, err := parseFragment(first)
	if err != nil {
		return nil, err
	}
	if int(total) > maxFragments {
		return nil, fmt.Errorf("a2s: split reply claims %d fragments", total)
	}
	got := map[byte]fragment{frag.number: frag}
	for len(got) < int(total) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next, err := c.readDatagram(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: %d of %d fragments: %v", ErrIncomplete, len(got), total, err)
		}
		if !bytesHavePrefix(next, splitPacketHeader) {
			continue // a late single-packet reply to an earlier request
		}
		nid, ntotal, nfrag, err := parseFragment(next)
		if err != nil {
			return nil, err
		}
		if nid != id || ntotal != total {
			continue // belongs to a different reply
		}
		got[nfrag.number] = nfrag
	}
	nums := make([]int, 0, len(got))
	for n := range got {
		nums = append(nums, int(n))
	}
	sort.Ints(nums)
	var out []byte
	for _, n := range nums {
		out = append(out, got[byte(n)].body...)
	}
	// The reassembled payload carries the single-packet header of the original
	// reply.
	if bytesHavePrefix(out, singlePacketHeader) {
		return out[4:], nil
	}
	return out, nil
}

func parseFragment(b []byte) (id uint32, total byte, f fragment, err error) {
	// -2 header(4) + id(4) + total(1) + number(1) + split size(2)
	if len(b) < 12 {
		return 0, 0, fragment{}, errors.New("a2s: short split header")
	}
	id = binary.LittleEndian.Uint32(b[4:8])
	total = b[8]
	number := b[9]
	if total == 0 || number >= total {
		return 0, 0, fragment{}, fmt.Errorf("a2s: bad fragment %d/%d", number, total)
	}
	// The compression bit is the id's high bit. Source 2 never compresses, and
	// guessing at a bzip2 payload would corrupt the result silently.
	if id&0x80000000 != 0 {
		return 0, 0, fragment{}, errors.New("a2s: compressed split replies are not supported")
	}
	return id, total, fragment{number: number, body: b[12:]}, nil
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

// Info carries the fields cs2a cares about from A2S_INFO. The JSON names are
// explicit because this struct is serialized into the agent's status payload.
type Info struct {
	Protocol byte   `json:"protocol"`
	Name     string `json:"name"`
	Map      string `json:"map"`
	Game     string `json:"game"`
	Players  int    `json:"players"`
	Max      int    `json:"max"`
	Bots     int    `json:"bots"`
}

// decodeInfo parses the Source A2S_INFO body. A truncated reply returns the
// zero Info alongside the error: a half-filled struct reached the panel and was
// rendered as a server named "" on map "", which looked like a healthy but
// unnamed server rather than a failed query.
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
	// VAC, version and the extra-data fields follow. They are not read: nothing
	// here needs them, and requiring them would turn a slightly short reply from
	// a proxy into a failed query for data that already arrived intact.
	if err := r.fail(); err != nil {
		return Info{}, err
	}
	return info, nil
}

// decodeGoldsrcInfo parses the obsolete 'm' reply. Its first field is the
// server address string, which the old decoder skipped — so every later field
// was read one string early and the map name came back as the server name.
func decodeGoldsrcInfo(b []byte) (Info, error) {
	var info Info
	r := &reader{b: b}
	r.str()             // address ("1.2.3.4:27015")
	info.Name = r.str() // hostname
	info.Map = r.str()
	r.str() // folder
	r.str() // game
	info.Players = int(r.byteVal())
	info.Max = int(r.byteVal())
	info.Protocol = r.byteVal()
	r.byteVal() // server type
	r.byteVal() // environment
	r.byteVal() // visibility
	if err := r.fail(); err != nil {
		return Info{}, err
	}
	return info, nil
}

// Player is one A2S_PLAYER row.
//
// The reply carries no SteamID: the fields are index, name, score and duration,
// full stop. The decoder used to consume eight bytes for one anyway, which
// desynchronized the stream after the first player — the second name was read
// out of the middle of the previous row's binary fields.
type Player struct {
	Index    byte    `json:"index"`
	Name     string  `json:"name"`
	Score    int32   `json:"score"`
	Duration float32 `json:"duration"` // seconds connected
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
		p.Duration = math.Float32frombits(r.u32())
		out = append(out, p)
	}
	if err := r.fail(); err != nil {
		// The rows decoded before the truncation are still meaningful, and the
		// error says the list is short.
		return out, err
	}
	return out, nil
}
