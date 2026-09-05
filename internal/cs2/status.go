package cs2

import (
	"regexp"
	"strconv"
	"strings"
)

// Player is one human/bot row of the RCON status output.
//
// The JSON names are explicit because this struct crosses the agent→panel API
// boundary: without tags Go emitted "SteamID"/"UserID", which the panel's
// snake_case decoder silently dropped. Every player then arrived with an empty
// SteamID, and the panel's "no SteamID means bot" rule labelled every human BOT.
type Player struct {
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	SteamID   string `json:"steam_id"` // normalized steamid64; empty for bots and on CS2, which omits it
	Addr      string `json:"addr,omitempty"`
	Connected string `json:"connected"` // "12:34" as reported; empty for bots
	Ping      int    `json:"ping"`
	Loss      int    `json:"loss"`
	State     string `json:"state"`
	Bot       bool   `json:"bot"`
}

// Status is the live state of the CS2 server as reported by RCON.
type Status struct {
	Hostname string   `json:"hostname"`
	Map      string   `json:"map"`
	Players  []Player `json:"players"`
	Humans   int      `json:"humans"`
	Bots     int      `json:"bots"`
	Max      int      `json:"max"`
}

// The engine prefixes console lines with a channel tag ("[Client] hostname : x")
// when status is read from a listen server's console. A dedicated server answers
// RCON without the tag, but tolerating it costs nothing.
const linePrefix = `^(?:\[[^\]]{1,32}\]\s*)?\s*`

var (
	reMap  = regexp.MustCompile(`(?m)` + linePrefix + `map\s*:\s*(\S+)`)
	reHost = regexp.MustCompile(`(?m)` + linePrefix + `hostname\s*:\s*(.+?)\s*$`)

	// CS2:    players  : 0 humans, 1 bots (0 max) (not hibernating) (unreserved)
	// CS:GO:  players  : 5 humans, 1 bots (10/0 max)
	// The legacy form prints slots/reserved, and requiring a single number made
	// the whole line fail to match, so humans and bots read 0 on those servers.
	rePlayers = regexp.MustCompile(`(?mi)` + linePrefix +
		`players\s*:\s*(\d+)\s*humans?,\s*(\d+)\s*bots?\s*\(\s*(\d+)\s*(?:/\s*(\d+)\s*)?max`)

	// CS2 has no "map : <name>" line at all. The loaded map is only named in the
	// spawngroup list, whose first entry is the map's own main lump:
	//   loaded spawngroup(  1)  : SV:  [1: de_dust2 | main lump | mapload]
	// Later entries are prefab paths ("maps/prefabs/de_dust2/..."), which is why
	// a candidate containing a slash is skipped.
	reSpawnGroup = regexp.MustCompile(`(?m)^\s*loaded spawngroup\(.*?\[\s*\d+:\s*([^|\]]+?)\s*[|\]]`)

	// Legacy (CS:GO / SourceTV) table, one row per client:
	//   # 2 "Player One" [U:1:1234567] 12:34 45 0 active
	reLegacyRow = regexp.MustCompile(`(?m)^#\s+(\d+)\s+"([^"]*)"\s+(\S+)\s+(\S+)\s+(\d+)\s+(\d+)\s+(\S+)`)

	// CS2 table, printed under "---------players--------" with the header
	//   id     time ping loss      state   rate adr name
	// and rows that carry neither a leading '#' nor a SteamID:
	//    0    00:18   49    0     active 786432 5.129.143.9:61274 'ZUBOR_'
	//    1      BOT    0    0     active      0 'Sam'
	// The name is single-quoted and last; a bot has no address and prints BOT
	// where a human prints its connected time.
	reCS2Row = regexp.MustCompile(`^\s*(\d+)\s+(\S+)\s+(\d+)\s+(\d+)\s+(\S+)\s+(\d+)(?:\s+(\S+))?\s+'(.*)'\s*$`)
)

// ParseStatus parses the output of the RCON "status" command. Parsing is
// deliberately lenient: unknown line shapes are skipped, known fields are
// extracted wherever they appear, and both the CS2 and the legacy Source table
// layouts are understood — CS2 changed the player table completely, so a parser
// that only knew the legacy shape reported an empty server no matter who was
// connected.
func ParseStatus(out string) Status {
	st := Status{}
	if m := reHost.FindStringSubmatch(out); m != nil {
		st.Hostname = strings.TrimSpace(m[1])
	}
	if m := reMap.FindStringSubmatch(out); m != nil {
		st.Map = m[1]
	} else {
		st.Map = mapFromSpawnGroups(out)
	}
	if m := rePlayers.FindStringSubmatch(out); m != nil {
		st.Humans, _ = strconv.Atoi(m[1])
		st.Bots, _ = strconv.Atoi(m[2])
		st.Max, _ = strconv.Atoi(m[3])
	}
	st.Players = parsePlayerRows(out)
	// CS2 always reports "(0 max)" and, when the summary line is missing
	// entirely, nothing at all. The rows are the more reliable count.
	if len(st.Players) > 0 {
		humans, bots := 0, 0
		for _, p := range st.Players {
			if p.Bot {
				bots++
			} else {
				humans++
			}
		}
		if humans+bots > st.Humans+st.Bots {
			st.Humans, st.Bots = humans, bots
		}
	}
	return st
}

// mapFromSpawnGroups recovers the map name from the CS2 spawngroup list.
func mapFromSpawnGroups(out string) string {
	for _, m := range reSpawnGroup.FindAllStringSubmatch(out, -1) {
		name := strings.TrimSpace(m[1])
		// Prefab spawngroups are paths; the map's own lump is a bare name.
		if name == "" || strings.ContainsAny(name, "/\\") {
			continue
		}
		return name
	}
	return ""
}

// parsePlayerRows reads the player table in either layout. The CS2 table is
// scoped to its section so the SourceTV status block (which prints the legacy
// header again) cannot inject phantom rows.
func parsePlayerRows(out string) []Player {
	var players []Player
	for _, m := range reLegacyRow.FindAllStringSubmatch(out, -1) {
		p := Player{
			UserID:    m[1],
			Name:      m[2],
			SteamID:   m[3],
			Connected: m[4],
			State:     m[7],
		}
		p.Ping, _ = strconv.Atoi(m[5])
		p.Loss, _ = strconv.Atoi(m[6])
		if norm, err := NormalizeSteamID(p.SteamID); err == nil {
			p.SteamID = norm
		} else {
			p.SteamID = "" // bots print "BOT" or an unparseable id
			p.Bot = true
		}
		players = append(players, p)
	}
	players = append(players, parseCS2Rows(out)...)
	return players
}

func parseCS2Rows(out string) []Player {
	var players []Player
	inSection := false
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		switch {
		case strings.HasPrefix(trimmed, "---------players"):
			inSection = true
			continue
		case !inSection:
			continue
		case trimmed == "" || trimmed == "#end" || strings.HasPrefix(trimmed, "---"):
			// Any other banner or the table terminator ends the section.
			inSection = false
			continue
		}
		m := reCS2Row.FindStringSubmatch(trimmed)
		if m == nil {
			continue // the column header and anything unexpected
		}
		p := Player{
			UserID: m[1],
			Name:   m[8],
			Addr:   m[7],
			State:  m[5],
		}
		p.Ping, _ = strconv.Atoi(m[3])
		p.Loss, _ = strconv.Atoi(m[4])
		// A bot prints "BOT" in the time column and has no address. CS2 does not
		// print a SteamID for anyone, so identity cannot come from this table.
		if strings.EqualFold(m[2], "BOT") || p.Addr == "" {
			p.Bot = true
		} else {
			p.Connected = m[2]
		}
		players = append(players, p)
	}
	return players
}
