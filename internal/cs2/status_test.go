package cs2

import "testing"

// A verbatim CS2 status dump (build 1.39.9.6, one bot on de_dust2 plus SourceTV).
// Two things about it broke the old parser completely: the player table has no
// leading '#', no SteamID and single-quoted names, and there is no "map :" line
// at all — the map is only named inside the spawngroup list.
const cs2StatusBotOnly = `Server:  Running [127.0.1.1:27015]
Client:  Disconnected
----- Status -----
@ Current  :  game
source   :  console
hostname :  Counter-Strike 2 Dedicated Server
spawngroup count : 1
version  : 1.39.9.6/13996 9961 secure  public
steamid  : [A:1:2512262174:28597] (90194818239435806)
udp/ip   : 127.0.1.1:27015 (public 203.0.113.9:27015)
os/type  : Linux dedicated
sourcetv[0] : 127.0.1.1:27020 (public 203.0.113.9:27020) delay 30.0s
players  : 0 humans, 1 bots (0 max) (not hibernating) (unreserved)
---------spawngroups----
loaded spawngroup(  1)  : SV:  [1: de_dust2 | main lump | mapload]
loaded spawngroup(  2)  : SV:  [2: maps/prefabs/de_dust2/de_dust2_skybox | main lump | mapload | point_prefab]
loaded spawngroup(  3)  : SV:  [3: prefabs/misc/team_select | main lump | mapload | point_prefab]
---------players--------
  id     time ping loss      state   rate adr name
   0      BOT    0    0     active      0 'SourceTV'
#end
--- SourceTV[0] Status ---
Online 00:08, FPS 63.7, Version 9961 (Linux)
Master "SourceTV", delay 30, frames 528 / ticks 531
Game Time 00:01, Mod "csgo", Map "de_dust2"
Local IP 127.0.1.1:27020, KB/sec In 0.0, Out 0.0
Local Slots 64, Spectators 0, Proxies 0
Total Slots 64, Spectators 0, Proxies 0
# userid name uniqueid connected ping loss state rate adr
#end
`

// A CS2 dump with a real human connected, plus bots.
const cs2StatusWithHuman = `hostname :  zombie server
version  : 1.40.5.5/14055 10108 secure  public
players  : 1 humans, 9 bots (0 max) (not hibernating) (unreserved)
---------spawngroups----
loaded spawngroup(  1)  : SV:  [1: de_mirage | main lump | mapload]
---------players--------
  id     time ping loss      state   rate adr name
   0    00:18   49    0     active 786432 5.129.143.9:61274 'ZUBOR_'
   1      BOT    0    0     active      0 'Sam'
   2      BOT    0    0     active      0 'Severn'
#end
`

func TestParseStatusCS2(t *testing.T) {
	st := ParseStatus(cs2StatusBotOnly)
	if st.Hostname != "Counter-Strike 2 Dedicated Server" {
		t.Errorf("hostname = %q", st.Hostname)
	}
	// CS2 prints no "map :" line; the name comes from the spawngroup list, and a
	// prefab path must not be mistaken for the map.
	if st.Map != "de_dust2" {
		t.Errorf("map = %q, want de_dust2", st.Map)
	}
	if len(st.Players) != 1 {
		t.Fatalf("players = %+v, want 1 row", st.Players)
	}
	p := st.Players[0]
	if p.Name != "SourceTV" || !p.Bot || p.SteamID != "" {
		t.Errorf("row = %+v", p)
	}
	if st.Humans != 0 || st.Bots != 1 {
		t.Errorf("humans/bots = %d/%d, want 0/1", st.Humans, st.Bots)
	}
}

func TestParseStatusCS2WithHuman(t *testing.T) {
	st := ParseStatus(cs2StatusWithHuman)
	if st.Map != "de_mirage" {
		t.Errorf("map = %q", st.Map)
	}
	if len(st.Players) != 3 {
		t.Fatalf("players = %d, want 3: %+v", len(st.Players), st.Players)
	}
	human := st.Players[0]
	if human.Name != "ZUBOR_" {
		t.Errorf("name = %q", human.Name)
	}
	if human.Bot {
		t.Error("a connected human was classified as a bot")
	}
	if human.Connected != "00:18" || human.Ping != 49 || human.Loss != 0 {
		t.Errorf("row = %+v", human)
	}
	if human.Addr != "5.129.143.9:61274" {
		t.Errorf("addr = %q", human.Addr)
	}
	if human.State != "active" {
		t.Errorf("state = %q", human.State)
	}
	for _, p := range st.Players[1:] {
		if !p.Bot || p.Addr != "" {
			t.Errorf("bot row = %+v", p)
		}
	}
	if st.Humans != 1 || st.Bots != 9 {
		t.Errorf("humans/bots = %d/%d, want 1/9", st.Humans, st.Bots)
	}
}

// A quoted name containing spaces, an apostrophe or a bracket must survive, and
// the SourceTV block's legacy header must not add phantom rows.
func TestParseStatusCS2AwkwardNames(t *testing.T) {
	out := `players  : 2 humans, 0 bots (0 max)
---------players--------
  id     time ping loss      state   rate adr name
   0    01:02   12    0     active 786432 198.51.100.7:27005 'it's me [tag]'
   1    00:04    9    1  spawning 786432 198.51.100.8:27005 'ᴜɴɪᴄᴏᴅᴇ ᴘʟᴀʏᴇʀ'
#end
--- SourceTV[0] Status ---
# userid name uniqueid connected ping loss state rate adr
#end
`
	st := ParseStatus(out)
	if len(st.Players) != 2 {
		t.Fatalf("players = %d: %+v", len(st.Players), st.Players)
	}
	if st.Players[0].Name != "it's me [tag]" {
		t.Errorf("name0 = %q", st.Players[0].Name)
	}
	if st.Players[1].Name != "ᴜɴɪᴄᴏᴅᴇ ᴘʟᴀʏᴇʀ" || st.Players[1].State != "spawning" {
		t.Errorf("row1 = %+v", st.Players[1])
	}
}

// The legacy "(20/0 max)" form (CS:GO and some proxies) used to make the whole
// summary line fail to match, so humans, bots and max all read 0.
func TestParseStatusLegacySlotsForm(t *testing.T) {
	st := ParseStatus("hostname: legacy\nmap     : de_dust2\nplayers : 5 humans, 1 bots (10/0 max)\n")
	if st.Humans != 5 || st.Bots != 1 || st.Max != 10 {
		t.Fatalf("humans/bots/max = %d/%d/%d, want 5/1/10", st.Humans, st.Bots, st.Max)
	}
	if st.Map != "de_dust2" {
		t.Errorf("map = %q", st.Map)
	}
}

// Console channel tags ("[Client] hostname : x") appear when status is read from
// a listen server's console.
func TestParseStatusTaggedLines(t *testing.T) {
	st := ParseStatus("[EngineServiceManager] ----- Status -----\n[Client] hostname :  tagged\n[Client] players  : 1 humans, 0 bots (0 max) (not hibernating) (unreserved)\n")
	if st.Hostname != "tagged" {
		t.Errorf("hostname = %q", st.Hostname)
	}
	if st.Humans != 1 {
		t.Errorf("humans = %d", st.Humans)
	}
}

// The map name must not be taken from a prefab spawngroup when the map's own
// lump is missing from the output (a truncated status).
func TestParseStatusSkipsPrefabSpawnGroups(t *testing.T) {
	out := `---------spawngroups----
loaded spawngroup(  2)  : SV:  [2: maps/prefabs/de_nuke/de_nuke_skybox | main lump | mapload | point_prefab]
`
	if st := ParseStatus(out); st.Map != "" {
		t.Fatalf("map = %q, want empty rather than a prefab path", st.Map)
	}
}
