package cs2

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeSteamID(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"[U:1:1234567]", "76561197961500295", false},
		{"U:1:1234567", "76561197961500295", false}, // pasted without brackets
		{"STEAM_1:0:11101", "76561197960287930", false},
		{"STEAM_0:1:11101", "76561197960287931", false},
		{"76561198029989895", "76561198029989895", false},
		{"11101", "76561197960276829", false}, // bare account number
		{" 76561198029989895 ", "76561198029989895", false},
		{"", "", true},
		{"0", "", true},
		{"not-a-steamid", "", true},
		{"https://steamcommunity.com/profiles/76561198029989895", "", true},
		{"STEAM_1:2:11101", "", true},
		{"[U:1:0]", "", true},
		// A clan id is not a player: converting it produced a SteamID64 that can
		// never match anyone, so the whitelist entry silently did nothing.
		{"[C:1:1234567]", "", true},
		// A group/gameserver SteamID64 is out of the individual account range.
		{"103582791429521408", "", true},
		{"18446744073709551615", "", true},
		// A non-public universe maps to a completely different SteamID64.
		{"STEAM_9:0:11101", "", true},
		{"[U:9:1234567]", "", true},
		// A truncated paste must not be treated as valid.
		{"[U:1:1234567", "", true},
		{"[U:1:]", "", true},
		{"STEAM_1:0:", "", true},
		{"STEAM_0:0:0", "", true},
	}
	for _, c := range cases {
		got, err := NormalizeSteamID(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizeSteamID(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeSteamID(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeSteamID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

const sampleStatus = `hostname: cs2a community server
version : 1.40.7.7/14077 11244 secure
udp/ip  :  192.0.2.10:27015
os      :  Linux
type    :  community dedicated
map     : de_dust2
players : 2 humans, 1 bots (12 max)

# userid name uniqueid connected ping loss state adr
# 2 "Player One" [U:1:1234567] 12:34 45 0 active
# 3 "Sneakybot" BOT 04:01 0 0 active
# 4 "Player Two" [U:1:7654321] 00:12 88 0 active
`

func TestParseStatus(t *testing.T) {
	st := ParseStatus(sampleStatus)
	if st.Hostname != "cs2a community server" {
		t.Errorf("hostname = %q", st.Hostname)
	}
	if st.Map != "de_dust2" {
		t.Errorf("map = %q", st.Map)
	}
	if st.Humans != 2 || st.Bots != 1 || st.Max != 12 {
		t.Errorf("players = %d/%d bots %d", st.Humans, st.Bots, st.Max)
	}
	if len(st.Players) != 3 {
		t.Fatalf("got %d players, want 3", len(st.Players))
	}
	if st.Players[0].Name != "Player One" || st.Players[0].SteamID != "76561197961500295" {
		t.Errorf("player0 = %+v", st.Players[0])
	}
	if st.Players[1].SteamID != "" {
		t.Errorf("bot steamid should be empty, got %q", st.Players[1].SteamID)
	}
	if st.Players[2].Ping != 88 {
		t.Errorf("player2 ping = %d", st.Players[2].Ping)
	}
}

func TestParseStatusEmpty(t *testing.T) {
	st := ParseStatus("")
	if st.Map != "" || len(st.Players) != 0 {
		t.Fatalf("expected zero status, got %+v", st)
	}
}

func TestManagedBlockRoundTrip(t *testing.T) {
	userCfg := "hostname \"my cool server\"\nrcon_password \"keepme\"\nsv_lan 0\n"
	settings := []CFGSetting{
		{Name: "sv_password", Value: "roundtrip", Comment: "set via cs2a panel"},
		{Name: "mp_maxrounds", Value: "24"},
	}
	applied := ApplyManagedBlock(userCfg, settings)

	// user content preserved
	for _, want := range []string{`hostname "my cool server"`, `rcon_password "keepme"`, "sv_lan 0"} {
		if !contains(applied, want) {
			t.Fatalf("user line lost after apply:\n%s", applied)
		}
	}
	got := ExtractManagedBlock(applied)
	if len(got) != 2 || got[0].Name != "sv_password" || got[0].Value != "roundtrip" {
		t.Fatalf("extract = %+v", got)
	}

	// re-apply with different values: no duplication, values updated
	settings2 := []CFGSetting{{Name: "sv_password", Value: "changed"}}
	again := ApplyManagedBlock(applied, settings2)
	if count := stringsCount(again, ManagedBlockBegin); count != 1 {
		t.Fatalf("managed block duplicated (%d)", count)
	}
	got2 := ExtractManagedBlock(again)
	if len(got2) != 1 || got2[0].Value != "changed" {
		t.Fatalf("extract2 = %+v", got2)
	}
	if !contains(again, `rcon_password "keepme"`) {
		t.Fatal("user rcon_password lost on re-apply")
	}
}

func TestSanitizeValue(t *testing.T) {
	got := ApplyManagedBlock("", []CFGSetting{{Name: "hostname", Value: `bad "quote"\nnew`}})
	// embedded quotes are stripped so the value stays inside its own quoted
	// argument; nothing may be escaped out
	if contains(got, `\"`) || contains(got, `quote"`) || contains(got, `"quote`) {
		t.Fatalf("value not sanitized:\n%s", got)
	}
	if !contains(got, `hostname "bad quote\nnew"`) {
		t.Fatalf("unexpected render:\n%s", got)
	}
}

func TestSaveLoadServerCFG(t *testing.T) {
	dir := t.TempDir()
	if err := SaveServerCFG(dir, "hostname \"x\"\n"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadServerCFG(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "hostname \"x\"\n" {
		t.Fatalf("roundtrip = %q", got)
	}
	// leftover temp files must not accumulate
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("temp files leaked: %d entries", len(ents))
	}
}

func TestLoadServerCFGMissing(t *testing.T) {
	if _, err := LoadServerCFG(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func contains(haystack, needle string) bool { return stringsCount(haystack, needle) > 0 }

func stringsCount(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); {
		j := indexOf(s[i:], sub)
		if j < 0 {
			break
		}
		n++
		i += j + len(sub)
	}
	return n
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
