package a2s

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// rawUDP is a fake that writes exactly the datagrams a test dictates, for the
// cases the well-behaved fake cannot express (split replies, oversized replies,
// silence).
type rawUDP struct {
	pc *net.UDPConn
}

func newRawUDP(t *testing.T, handle func(pc *net.UDPConn, raddr *net.UDPAddr, req []byte)) *rawUDP {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := &rawUDP{pc: pc}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, raddr, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req := append([]byte{}, buf[:n]...)
			handle(pc, raddr, req)
		}
	}()
	t.Cleanup(func() { _ = pc.Close() })
	return s
}

func (s *rawUDP) addr() string { return s.pc.LocalAddr().String() }

// splitReply chops a whole reply payload (header included) into Source-format
// fragments.
func splitReply(id uint32, payload []byte, size int) [][]byte {
	var out [][]byte
	total := (len(payload) + size - 1) / size
	for i := 0; i < total; i++ {
		end := (i + 1) * size
		if end > len(payload) {
			end = len(payload)
		}
		head := make([]byte, 12)
		copy(head, splitPacketHeader)
		binary.LittleEndian.PutUint32(head[4:8], id)
		head[8] = byte(total)
		head[9] = byte(i)
		binary.LittleEndian.PutUint16(head[10:12], uint16(size))
		out = append(out, append(head, payload[i*size:end]...))
	}
	return out
}

func infoPayload(name, mapName string, players, max, bots int) []byte {
	b := []byte{headerInfoResponse, 24}
	b = append(b, nullStr(name)...)
	b = append(b, nullStr(mapName)...)
	b = append(b, nullStr("csgo")...)
	b = append(b, nullStr("Counter-Strike 2")...)
	b = append(b, 0x00, 0x00)
	b = append(b, byte(players), byte(max), byte(bots), 'd', 'l', 0)
	return b
}

// A reply too large for one datagram arrives as fragments. The old client
// returned the first fragment's payload as the whole reply, so a server with a
// long hostname decoded as garbage — or, when the fragment happened to end on a
// string boundary, as a plausible but wrong Info.
func TestInfoReassemblesASplitReply(t *testing.T) {
	longName := strings.Repeat("cs2a community server ", 80) // > one datagram
	whole := append(append([]byte{}, singlePacketHeader...), infoPayload(longName, "de_nuke", 12, 24, 2)...)
	frags := splitReply(0x1234, whole, 900)
	if len(frags) < 2 {
		t.Fatalf("expected several fragments, got %d", len(frags))
	}
	s := newRawUDP(t, func(pc *net.UDPConn, raddr *net.UDPAddr, req []byte) {
		if len(req) < 5 || req[4] != headerInfoRequest {
			return
		}
		for _, f := range frags {
			_, _ = pc.WriteToUDP(f, raddr)
		}
	})
	c, err := Dial(s.addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Name != longName {
		t.Errorf("name truncated: %d of %d bytes", len(info.Name), len(longName))
	}
	if info.Map != "de_nuke" || info.Players != 12 || info.Max != 24 || info.Bots != 2 {
		t.Errorf("info = %+v", info)
	}
}

// Fragments may arrive out of order; UDP makes no promises.
func TestInfoReassemblesOutOfOrderFragments(t *testing.T) {
	name := strings.Repeat("abcdefghij", 200)
	whole := append(append([]byte{}, singlePacketHeader...), infoPayload(name, "de_train", 3, 10, 1)...)
	frags := splitReply(0xABCD, whole, 700)
	s := newRawUDP(t, func(pc *net.UDPConn, raddr *net.UDPAddr, req []byte) {
		if len(req) < 5 || req[4] != headerInfoRequest {
			return
		}
		for i := len(frags) - 1; i >= 0; i-- {
			_, _ = pc.WriteToUDP(frags[i], raddr)
		}
	})
	c, err := Dial(s.addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Name != name || info.Map != "de_train" {
		t.Errorf("out-of-order reassembly failed: map=%q name len=%d", info.Map, len(info.Name))
	}
}

// A split reply missing a fragment must be an error. Decoding what arrived
// produced a truncated map name that the panel displayed as fact.
func TestInfoMissingFragmentIsAnError(t *testing.T) {
	whole := append(append([]byte{}, singlePacketHeader...), infoPayload(strings.Repeat("x", 2000), "de_ancient", 1, 10, 0)...)
	frags := splitReply(0x55, whole, 800)
	s := newRawUDP(t, func(pc *net.UDPConn, raddr *net.UDPAddr, req []byte) {
		if len(req) < 5 || req[4] != headerInfoRequest {
			return
		}
		_, _ = pc.WriteToUDP(frags[0], raddr) // drop the rest
	})
	c, err := Dial(s.addr(), 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	info, err := c.Info(context.Background())
	if err == nil {
		t.Fatalf("a partial split reply decoded as success: %+v", info)
	}
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
}

// A single datagram larger than the old 4096-byte buffer must be read whole.
// A 64-slot A2S_PLAYER reply exceeds it, and the tail was silently discarded.
func TestPlayersReadsALargeDatagram(t *testing.T) {
	const players = 64
	body := []byte{headerPlayerResponse, players}
	for i := 0; i < players; i++ {
		body = append(body, byte(i))
		body = append(body, nullStr(strings.Repeat("n", 60)+string(rune('a'+i%26)))...)
		var num [4]byte
		binary.LittleEndian.PutUint32(num[:], uint32(i))
		body = append(body, num[:]...)
		binary.LittleEndian.PutUint32(num[:], 0x42480000)
		body = append(body, num[:]...)
	}
	if len(body) <= 4096 {
		t.Fatalf("test datagram is only %d bytes; it must exceed the old buffer", len(body))
	}
	s := newRawUDP(t, func(pc *net.UDPConn, raddr *net.UDPAddr, req []byte) {
		if len(req) < 5 || req[4] != headerPlayerRequest {
			return
		}
		_, _ = pc.WriteToUDP(append(append([]byte{}, singlePacketHeader...), body...), raddr)
	})
	c, err := Dial(s.addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ps, err := c.Players(context.Background())
	if err != nil {
		t.Fatalf("players: %v", err)
	}
	if len(ps) != players {
		t.Fatalf("got %d players, want %d", len(ps), players)
	}
	if ps[players-1].Name == "" {
		t.Error("last player row is empty; the datagram was truncated")
	}
}

// Consecutive player rows must line up. The decoder consumed a nonexistent
// 8-byte SteamID per row, so the second name was read from the middle of the
// first row's binary fields.
func TestPlayersDecodeConsecutiveRows(t *testing.T) {
	body := []byte{headerPlayerResponse, 3}
	names := []string{"alpha", "bravo", "charlie"}
	for i, n := range names {
		body = append(body, byte(i))
		body = append(body, nullStr(n)...)
		var num [4]byte
		binary.LittleEndian.PutUint32(num[:], uint32((i+1)*10))
		body = append(body, num[:]...)
		binary.LittleEndian.PutUint32(num[:], 0x42480000) // 50.0f
		body = append(body, num[:]...)
	}
	s := newRawUDP(t, func(pc *net.UDPConn, raddr *net.UDPAddr, req []byte) {
		if len(req) < 5 || req[4] != headerPlayerRequest {
			return
		}
		_, _ = pc.WriteToUDP(append(append([]byte{}, singlePacketHeader...), body...), raddr)
	})
	c, err := Dial(s.addr(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ps, err := c.Players(context.Background())
	if err != nil {
		t.Fatalf("players: %v", err)
	}
	if len(ps) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(ps), ps)
	}
	for i, want := range names {
		if ps[i].Name != want {
			t.Errorf("row %d name = %q, want %q", i, ps[i].Name, want)
		}
		if ps[i].Score != int32((i+1)*10) {
			t.Errorf("row %d score = %d", i, ps[i].Score)
		}
		if ps[i].Duration < 49.9 || ps[i].Duration > 50.1 {
			t.Errorf("row %d duration = %v", i, ps[i].Duration)
		}
	}
}

// A truncated A2S_INFO must not return a half-filled struct: the panel rendered
// it as a nameless server on no map, which reads as "running fine" rather than
// "the query failed".
func TestInfoTruncatedReturnsNoPartialStruct(t *testing.T) {
	body := []byte{headerInfoResponse, 24}
	body = append(body, nullStr("half a server")...)
	// the map string is cut off mid-way, with no terminator
	body = append(body, []byte("de_dus")...)
	s := newRawUDP(t, func(pc *net.UDPConn, raddr *net.UDPAddr, req []byte) {
		if len(req) < 5 || req[4] != headerInfoRequest {
			return
		}
		_, _ = pc.WriteToUDP(append(append([]byte{}, singlePacketHeader...), body...), raddr)
	})
	c, err := Dial(s.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	info, err := c.Info(context.Background())
	if err == nil {
		t.Fatal("want an error for a truncated info reply")
	}
	if info.Name != "" || info.Map != "" {
		t.Fatalf("partial info leaked to the caller: %+v", info)
	}
}

// The GoldSrc 'm' reply starts with the server address. Skipping it read every
// later field one string early, so the hostname came back as "1.2.3.4:27015".
func TestGoldsrcInfoStartsWithTheAddress(t *testing.T) {
	body := []byte{headerInfoGoldsrc}
	body = append(body, nullStr("192.0.2.10:27015")...)
	body = append(body, nullStr("old school server")...)
	body = append(body, nullStr("de_dust")...)
	body = append(body, nullStr("cstrike")...)
	body = append(body, nullStr("Counter-Strike")...)
	body = append(body, 5, 16, 47, 'd', 'l', 0)
	s := newRawUDP(t, func(pc *net.UDPConn, raddr *net.UDPAddr, req []byte) {
		if len(req) < 5 || req[4] != headerInfoRequest {
			return
		}
		_, _ = pc.WriteToUDP(append(append([]byte{}, singlePacketHeader...), body...), raddr)
	})
	c, err := Dial(s.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Name != "old school server" || info.Map != "de_dust" {
		t.Fatalf("info = %+v", info)
	}
	if info.Players != 5 || info.Max != 16 {
		t.Errorf("players = %d/%d", info.Players, info.Max)
	}
}

// A cancelled context must stop the query. The socket timeout alone let a read
// run past the caller's deadline, so a status request could overrun the panel's
// own limit.
func TestInfoRespectsTheContextDeadline(t *testing.T) {
	s := newRawUDP(t, func(pc *net.UDPConn, raddr *net.UDPAddr, req []byte) {
		// never answer
	})
	c, err := Dial(s.addr(), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Info(ctx); err == nil {
		t.Fatal("want an error when the context expires")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Info took %v; the context deadline was ignored", elapsed)
	}
}

// A compressed split reply cannot be decoded, so it must be reported rather
// than fed to the parser as if it were plain bytes.
func TestCompressedSplitReplyIsRejected(t *testing.T) {
	whole := append(append([]byte{}, singlePacketHeader...), infoPayload("x", "de_dust2", 1, 10, 0)...)
	frags := splitReply(0x80000001, whole, 40) // high bit = compressed
	s := newRawUDP(t, func(pc *net.UDPConn, raddr *net.UDPAddr, req []byte) {
		if len(req) < 5 || req[4] != headerInfoRequest {
			return
		}
		for _, f := range frags {
			_, _ = pc.WriteToUDP(f, raddr)
		}
	})
	c, err := Dial(s.addr(), 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Info(context.Background()); err == nil {
		t.Fatal("a compressed reply was decoded as plain data")
	}
}
