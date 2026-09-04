package a2s

import (
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"testing"
	"time"
)

const fakeChallenge = uint32(0x11223344)

// fakeA2S answers A2S_INFO / A2S_PLAYER on a loopback UDP socket using the
// real wire format: [FF FF FF FF][header][payload].
type fakeA2S struct {
	t  *testing.T
	pc *net.UDPConn

	name    string
	mapName string
	players int
	max     int
}

func startFakeA2S(t *testing.T, name, mapName string, players, max int) *fakeA2S {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	f := &fakeA2S{t: t, pc: pc.(*net.UDPConn), name: name, mapName: mapName, players: players, max: max}
	go f.serve()
	t.Cleanup(func() { _ = pc.Close() })
	return f
}

func (f *fakeA2S) addr() string { return f.pc.LocalAddr().String() }

func (f *fakeA2S) serve() {
	buf := make([]byte, 2048)
	for {
		n, raddr, err := f.pc.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 5 || !isHeaderPrefix(buf[:4]) {
			continue
		}
		switch buf[4] {
		case headerInfoRequest:
			f.send(raddr, f.infoResponse())
		case headerPlayerRequest:
			if n >= 9 && binary.LittleEndian.Uint32(buf[5:9]) != fakeChallenge {
				f.sendChallenge(raddr)
				continue
			}
			f.send(raddr, f.playersResponse())
		default:
			// ignore
		}
	}
}

func isHeaderPrefix(b []byte) bool {
	return b[0] == 0xFF && b[1] == 0xFF && b[2] == 0xFF && b[3] == 0xFF
}

func (f *fakeA2S) send(raddr *net.UDPAddr, payload []byte) {
	out := append([]byte{0xFF, 0xFF, 0xFF, 0xFF}, payload...)
	if _, err := f.pc.WriteToUDP(out, raddr); err != nil {
		f.t.Logf("fakeA2S send: %v", err)
	}
}

func (f *fakeA2S) sendChallenge(raddr *net.UDPAddr) {
	p := []byte{headerChallenge, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(p[1:5], fakeChallenge)
	f.send(raddr, p)
}

func (f *fakeA2S) infoResponse() []byte {
	var b []byte
	b = append(b, headerInfoResponse)
	b = append(b, 24)                    // protocol
	b = append(b, nullStr(f.name)...)    // name
	b = append(b, nullStr(f.mapName)...) // map
	b = append(b, nullStr("csgo")...)    // folder
	b = append(b, nullStr("Counter-Strike 2")...)
	b = append(b, 0x00, 0x00)      // appid (u16)
	b = append(b, byte(f.players)) // players
	b = append(b, byte(f.max))     // max
	b = append(b, 0)               // bots
	b = append(b, 'd')             // server type: dedicated
	b = append(b, 'l')             // environment: linux
	b = append(b, 0)               // visibility: public
	return b
}

func (f *fakeA2S) playersResponse() []byte {
	b := []byte{headerPlayerResponse, byte(f.players)}
	for i := 0; i < f.players; i++ {
		b = append(b, byte(i+1))
		b = append(b, nullStr("player"+strconv.Itoa(i+1))...)
		var score [4]byte
		binary.LittleEndian.PutUint32(score[:], uint32(100*(i+1)))
		b = append(b, score[:]...)
		var dur [4]byte
		binary.LittleEndian.PutUint32(dur[:], 0x42480000) // 50.0f
		b = append(b, dur[:]...)
		var sid [8]byte
		binary.LittleEndian.PutUint64(sid[:], 0)
		b = append(b, sid[:]...)
	}
	return b
}

func nullStr(s string) []byte {
	return append([]byte(s), 0)
}

// --- tests ------------------------------------------------------------

func TestInfo(t *testing.T) {
	f := startFakeA2S(t, "cs2a test server", "de_mirage", 3, 12)
	c, err := Dial(f.addr(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	info, err := c.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Name != "cs2a test server" || info.Map != "de_mirage" {
		t.Fatalf("info = %+v", info)
	}
	if info.Players != 3 || info.Max != 12 || info.Game != "Counter-Strike 2" {
		t.Fatalf("info = %+v", info)
	}
}

func TestPlayersWithChallenge(t *testing.T) {
	f := startFakeA2S(t, "cs2a", "de_dust2", 2, 10)
	c, err := Dial(f.addr(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	ps, err := c.Players(context.Background())
	if err != nil {
		t.Fatalf("players: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("got %d players, want 2", len(ps))
	}
	if ps[0].Name != "player1" || ps[0].Score != 100 {
		t.Fatalf("player0 = %+v", ps[0])
	}
	if ps[0].Duration < 49.9 || ps[0].Duration > 50.1 {
		t.Fatalf("duration = %v, want ~50", ps[0].Duration)
	}
}

func TestDialBadAddr(t *testing.T) {
	if _, err := Dial("not an address", time.Second); err == nil {
		t.Fatal("expected resolve error")
	}
}

func TestReaderTruncation(t *testing.T) {
	// decoding a truncated payload must not panic
	r := &reader{b: []byte{1, 2}}
	r.str()
	r.u16()
	r.u32()
	r.u64()
	r.skip(100)
	if r.fail() == nil {
		t.Fatal("expected truncation error")
	}
}
