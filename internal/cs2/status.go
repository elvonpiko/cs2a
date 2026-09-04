package cs2

import (
	"regexp"
	"strconv"
	"strings"
)

// Player is one human/bot row of the RCON status output.
type Player struct {
	UserID    string
	Name      string
	SteamID   string // normalized steamid64, "" for bots
	Connected string // "12:34" style duration as reported
	Ping      int
	State     string
}

// Status is the live state of the CS2 server as reported by RCON.
type Status struct {
	Hostname string
	Map      string
	Players  []Player
	Humans   int
	Bots     int
	Max      int
}

var (
	reMap     = regexp.MustCompile(`(?m)^\s*map\s*:\s*(\S+)`)
	reHost    = regexp.MustCompile(`(?m)^\s*hostname\s*:\s*(.+?)\s*$`)
	rePlayers = regexp.MustCompile(`(?mi)^\s*players\s*:\s*(\d+)\s*humans?,\s*(\d+)\s*bots?\s*\((\d+)\s*max[^)]*\)`)

	// # userid name uniqueid connected ping loss state ...
	// CS2 rows look like: # 2 "Player One" [U:1:1234567] 12:34 45 0 active
	rePlayerRow = regexp.MustCompile(`(?m)^#\s+(\d+)\s+"([^"]*)"\s+(\S+)\s+(\S+)\s+(\d+)\s+(\d+)\s+(\S+)`)
)

// ParseStatus parses the output of the RCON "status" command. Parsing is
// deliberately lenient: unknown line shapes are skipped, known fields are
// extracted wherever they appear.
func ParseStatus(out string) Status {
	st := Status{}
	if m := reHost.FindStringSubmatch(out); m != nil {
		st.Hostname = strings.TrimSpace(m[1])
	}
	if m := reMap.FindStringSubmatch(out); m != nil {
		st.Map = m[1]
	}
	if m := rePlayers.FindStringSubmatch(out); m != nil {
		st.Humans, _ = strconv.Atoi(m[1])
		st.Bots, _ = strconv.Atoi(m[2])
		st.Max, _ = strconv.Atoi(m[3])
	}
	for _, m := range rePlayerRow.FindAllStringSubmatch(out, -1) {
		p := Player{
			UserID:    m[1],
			Name:      m[2],
			SteamID:   m[3],
			Connected: m[4],
			State:     m[7],
		}
		p.Ping, _ = strconv.Atoi(m[5])
		if norm, err := NormalizeSteamID(p.SteamID); err == nil {
			p.SteamID = norm
		} else {
			p.SteamID = "" // bots print "BOT" or unparseable ids
		}
		st.Players = append(st.Players, p)
	}
	return st
}
