// Package cs2 contains domain helpers for managing a CS2 dedicated server:
// SteamID normalization, parsing of RCON "status" output, and safe editing of
// server.cfg via a managed block.
package cs2

import (
	"fmt"
	"strconv"
	"strings"
)

// steamID64Base is the universal steamid64 base account offset: the SteamID64
// of account number 0 in the public universe.
const steamID64Base uint64 = 76561197960265728

// maxAccountID is the largest 32-bit account number, so the highest SteamID64 a
// real individual account can have. Above it the value is not an individual
// account id — it is a clan, a game server or plain nonsense, and whitelisting
// it can never match a player.
const maxAccountID uint64 = 1<<32 - 1

// NormalizeSteamID converts the ways a player id shows up into a canonical
// SteamID64 string:
//
//	[U:1:1234567]        SteamID3 (with or without the closing bracket)
//	STEAM_1:0:12345      legacy SteamID2 (some logs/plugins)
//	76561198029989895    already a SteamID64
//	1234567              a bare account number
//
// Everything else is rejected. Being strict matters more than being helpful
// here: these ids drive the whitelist, and a value that is silently "corrected"
// into a different valid-looking SteamID64 either fails to admit the player the
// operator meant or, with enforcement on, locks them out of their own server.
func NormalizeSteamID(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return "", fmt.Errorf("empty steamid")
	case strings.HasPrefix(s, "[U:") || strings.HasPrefix(s, "U:"):
		return fromSteamID3(s, raw)
	case strings.HasPrefix(s, "STEAM_"):
		return fromSteamID2(s, raw)
	default:
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return "", fmt.Errorf("not a steamid: %q", raw)
		}
		if n >= steamID64Base {
			// A SteamID64 for an individual account: the account number is the
			// low 32 bits, so anything past that range is a different id type
			// (clan, game server, ...) wearing SteamID64 clothes.
			if n-steamID64Base > maxAccountID {
				return "", fmt.Errorf("not an individual steamid64: %q", raw)
			}
			return strconv.FormatUint(n, 10), nil
		}
		// Below the base: a bare account number. It must still fit in 32 bits,
		// or the sum is not a valid individual SteamID64 either.
		if n == 0 || n > maxAccountID {
			return "", fmt.Errorf("not a steamid: %q", raw)
		}
		return strconv.FormatUint(steamID64Base+n, 10), nil
	}
}

// fromSteamID3 parses [U:1:accountid] — the universe/type pair is checked
// rather than assumed. "[C:1:…]" (a clan) used to be accepted as a player and
// converted into a SteamID64 that can never appear in a status line.
func fromSteamID3(s, raw string) (string, error) {
	inner := strings.TrimPrefix(s, "[")
	if strings.HasSuffix(inner, "]") {
		inner = strings.TrimSuffix(inner, "]")
	} else if strings.HasPrefix(s, "[") {
		// An opening bracket with no closing one is a truncated paste.
		return "", fmt.Errorf("invalid steamid3 %q", raw)
	}
	parts := strings.Split(inner, ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid steamid3 %q", raw)
	}
	if parts[0] != "U" {
		return "", fmt.Errorf("steamid3 %q is not an individual account", raw)
	}
	// The universe: 1 is public. cs2a manages public servers, and a beta or dev
	// universe id would map to a completely different SteamID64.
	if parts[1] != "1" {
		return "", fmt.Errorf("unsupported steamid3 universe in %q", raw)
	}
	n, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil || n == 0 {
		return "", fmt.Errorf("invalid steamid3 %q", raw)
	}
	return strconv.FormatUint(steamID64Base+n, 10), nil
}

// fromSteamID2 parses STEAM_X:Y:Z. X is the universe: only 0 and 1 mean the
// public universe (0 appears in older CS:GO logs). "STEAM_9:0:1" used to be
// accepted and mapped onto a public-universe id.
func fromSteamID2(s, raw string) (string, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid legacy steamid %q", raw)
	}
	universe := strings.TrimPrefix(parts[0], "STEAM_")
	if universe != "0" && universe != "1" {
		return "", fmt.Errorf("unsupported steamid universe in %q", raw)
	}
	y, err1 := strconv.ParseUint(parts[1], 10, 8)
	z, err2 := strconv.ParseUint(parts[2], 10, 32)
	if err1 != nil || err2 != nil || (y != 0 && y != 1) {
		return "", fmt.Errorf("invalid legacy steamid %q", raw)
	}
	if z == 0 && y == 0 {
		return "", fmt.Errorf("invalid legacy steamid %q", raw)
	}
	account := z*2 + y
	if account > maxAccountID {
		return "", fmt.Errorf("invalid legacy steamid %q", raw)
	}
	return strconv.FormatUint(steamID64Base+account, 10), nil
}
