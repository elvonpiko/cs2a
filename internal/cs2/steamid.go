// Package cs2 contains domain helpers for managing a CS2 dedicated server:
// SteamID normalization, parsing of RCON "status" output, and safe editing of
// server.cfg via a managed block.
package cs2

import (
	"fmt"
	"strconv"
	"strings"
)

// steamID64Base is the universal steamid64 base account offset.
const steamID64Base uint64 = 76561197960265728

// NormalizeSteamID converts the many ways a player id shows up into a
// canonical SteamID64 string:
//
//	[U:1:1234567]        SteamID3 (CS2 RCON "status" format)
//	STEAM_1:0:12345      legacy SteamID2 (some logs/plugins)
//	76561198029989895    already a SteamID64
//
// It rejects anything else (profiles URLs, vanity names, ...).
func NormalizeSteamID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return "", fmt.Errorf("empty steamid")
	case strings.HasPrefix(s, "[U:1:"):
		inner := strings.TrimSuffix(strings.TrimPrefix(s, "[U:1:"), "]")
		n, err := strconv.ParseUint(inner, 10, 32)
		if err != nil || n == 0 {
			return "", fmt.Errorf("invalid steamid3 %q", raw)
		}
		return strconv.FormatUint(steamID64Base+n, 10), nil
	case strings.HasPrefix(s, "STEAM_"):
		parts := strings.Split(s, ":")
		if len(parts) != 3 {
			return "", fmt.Errorf("invalid legacy steamid %q", raw)
		}
		y, err1 := strconv.ParseUint(parts[1], 10, 8)
		z, err2 := strconv.ParseUint(parts[2], 10, 32)
		if err1 != nil || err2 != nil || (y != 0 && y != 1) {
			return "", fmt.Errorf("invalid legacy steamid %q", raw)
		}
		return strconv.FormatUint(steamID64Base+z*2+y, 10), nil
	default:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return "", fmt.Errorf("not a steamid: %q", raw)
		}
		// A SteamID64 lives in [76561197960265729, ...]; anything below the
		// base is an account number someone pasted raw.
		if n < steamID64Base {
			if n == 0 {
				return "", fmt.Errorf("not a steamid: %q", raw)
			}
			return strconv.FormatUint(steamID64Base+n, 10), nil
		}
		return strconv.FormatUint(n, 10), nil
	}
}
