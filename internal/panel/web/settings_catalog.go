package web

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The curated settings catalog: vanilla CS2 cvars a casual server operator
// actually wants, grouped the way the game presents them. Every entry is
// typed — the panel renders (and validates) an int, float, bool or one of a
// fixed set — because a free-text "value" field is how a typo like "thirty"
// ends up in server.cfg and silently ignored by the engine.
//
// What is deliberately NOT here:
//   - sv_password: the Access page owns it (it is access control, and its
//     save path reloads the map so enforcement is immediate). The settings
//     save below preserves whatever the managed block holds for it.
//   - Cheat-protected, engine or client-only cvars: they are refused or
//     ignored by the server even when written to server.cfg.
//   - mp_warmup_start / mp_warmup_end: those are commands, not cvars — they
//     fire the moment they are read, so a line in server.cfg would start
//     warmup on every exec. They are buttons on the settings page instead.

// SettingKind describes how one catalog entry is rendered and validated.
type SettingKind string

const (
	KindInt    SettingKind = "int"
	KindFloat  SettingKind = "float"
	KindBool   SettingKind = "bool"
	KindSelect SettingKind = "select"
	KindText   SettingKind = "text"
)

// SettingGroup is one card on the settings page.
type SettingGroup struct {
	Title string
	Blurb string
	Rows  []SettingSpec
}

// SettingSpec is one editable cvar.
type SettingSpec struct {
	Name  string
	Label string
	// Value/HasValue are filled by the page handler from the managed block:
	// HasValue=false means the cvar has never been set through cs2a (the
	// field then renders with the engine default as a placeholder hint).
	Value    string
	HasValue bool
	Kind     SettingKind
	// Min/Max bound int/float inputs (game limits, not arbitrary).
	Min, Max float64
	// Options for KindSelect: value → label.
	Options []SettingOption
	// Hint is the honest line under the field: what it does, and whether a
	// change applies live or needs a restart/map change.
	Hint string
}

type SettingOption struct {
	Value, Label string
}

func boolOption(v, l string) SettingOption { return SettingOption{v, l} }

// settingsCatalog is grouped in the order the page shows the cards.
func SettingsCatalog() []SettingGroup {
	return []SettingGroup{
		{
			Title: "Game",
			Blurb: "The mode the server runs. Changing this reloads the map — every match in progress ends.",
			Rows: []SettingSpec{
				{Name: "game_type", Label: "Game type", Kind: KindSelect, Options: []SettingOption{
					boolOption("0", "Classic (casual, competitive)"),
					boolOption("1", "Training"),
					boolOption("2", "Custom"),
				}, Hint: "Read at map load — saving changes game_mode with it, then reloads the map."},
				{Name: "game_mode", Label: "Game mode", Kind: KindSelect, Options: []SettingOption{
					boolOption("0", "Casual"),
					boolOption("1", "Competitive"),
					boolOption("2", "Wingman"),
					boolOption("3", "Arms Race"),
					boolOption("4", "Deathmatch"),
				}, Hint: "Classic (game_type 0) uses this. Saving reloads the map."},
			},
		},
		{
			Title: "Warmup",
			Blurb: "The free-for-all phase before the match starts. Length lives here; start/end are buttons below because they are commands, not settings.",
			Rows: []SettingSpec{
				{Name: "mp_warmuptime", Label: "Warmup length (seconds)", Kind: KindInt, Min: 5, Max: 600,
					Hint: "Applies to the next warmup phase. 0 disables the warmup timer."},
				{Name: "mp_warmuptime_pausetimer", Label: "Warmup stays until players are ready", Kind: KindBool,
					Hint: "Extends warmup while set — live immediately when the server is running."},
				{Name: "mp_warmup_pausetimer", Label: "Pause the current warmup timer", Kind: KindBool,
					Hint: "Freezes the countdown — live immediately."},
			},
		},
		{
			Title: "Rounds",
			Blurb: "How long a round runs and how many rounds a match lasts.",
			Rows: []SettingSpec{
				{Name: "mp_freezetime", Label: "Freeze time (seconds)", Kind: KindInt, Min: 0, Max: 60,
					Hint: "Buy time at round start. Applies from the next round — live immediately."},
				{Name: "mp_roundtime", Label: "Round time (minutes)", Kind: KindInt, Min: 1, Max: 60,
					Hint: "Round length. CS2 shows this live once set."},
				{Name: "mp_maxrounds", Label: "Max rounds (0 = unlimited)", Kind: KindInt, Min: 0, Max: 100,
					Hint: "First to half+1 wins. Applies to the next match — the current one keeps its limit."},
				{Name: "mp_round_restart_delay", Label: "Round restart delay (seconds)", Kind: KindFloat, Min: 0, Max: 30,
					Hint: "Time between the round ending and the next starting — live immediately."},
			},
		},
		{
			Title: "Economy",
			Blurb: "Player money: what they start with, the cap, and the per-round bonus.",
			Rows: []SettingSpec{
				{Name: "mp_startmoney", Label: "Starting money", Kind: KindInt, Min: 0, Max: 16000,
					Hint: "Money at spawn/reset. Applies from the next round."},
				{Name: "mp_maxmoney", Label: "Maximum money", Kind: KindInt, Min: 0, Max: 16000,
					Hint: "The cap a player can hold — live immediately."},
				{Name: "mp_afterroundmoney", Label: "Money after each round (0 = normal rules)", Kind: KindInt, Min: 0, Max: 16000,
					Hint: "Set above 0 to give every player this much each round end, win or lose."},
				{Name: "mp_buytime", Label: "Buy time (seconds)", Kind: KindInt, Min: 10, Max: 300,
					Hint: "How long the buy menu stays open. Applies from the next round."},
			},
		},
		{
			Title: "Bomb",
			Blurb: "C4 behaviour: the timer and who carries it.",
			Rows: []SettingSpec{
				{Name: "mp_c4timer", Label: "C4 timer (seconds)", Kind: KindInt, Min: 10, Max: 300,
					Hint: "Time from planting to detonation — live immediately."},
				{Name: "mp_give_player_c4", Label: "Give the C4 to a player", Kind: KindBool,
					Hint: "Off = the bomb stays on the ground at round start. Applies from the next round."},
			},
		},
		{
			Title: "Teams",
			Blurb: "Team balance and size limits.",
			Rows: []SettingSpec{
				{Name: "mp_autoteambalance", Label: "Auto-balance teams", Kind: KindBool,
					Hint: "Moves players between rounds when one side is stacked. Applies from the next round."},
				{Name: "mp_limitteams", Label: "Max team size difference (0 = no limit)", Kind: KindInt, Min: 0, Max: 8,
					Hint: "Blocks joining the bigger team beyond this — live immediately."},
			},
		},
		{
			Title: "Players",
			Blurb: "Friendly fire, damage scaling and gravity.",
			Rows: []SettingSpec{
				{Name: "mp_friendlyfire", Label: "Friendly fire", Kind: KindBool,
					Hint: "Teammates can damage each other — live immediately."},
				{Name: "mp_damage_scale_ct_head", Label: "CT head damage scale", Kind: KindFloat, Min: 0, Max: 10,
					Hint: "1 = normal. Halve it for tanky CTs — live immediately."},
				{Name: "mp_damage_scale_ct_body", Label: "CT body damage scale", Kind: KindFloat, Min: 0, Max: 10,
					Hint: "1 = normal — live immediately."},
				{Name: "mp_damage_scale_t_head", Label: "T head damage scale", Kind: KindFloat, Min: 0, Max: 10,
					Hint: "1 = normal — live immediately."},
				{Name: "mp_damage_scale_t_body", Label: "T body damage scale", Kind: KindFloat, Min: 0, Max: 10,
					Hint: "1 = normal — live immediately."},
				{Name: "sv_gravity", Label: "Gravity (800 = normal)", Kind: KindFloat, Min: 100, Max: 2000,
					Hint: "Lower = floatier jumps — live immediately."},
			},
		},
		{
			Title: "Server",
			Blurb: "How the server names itself and who can reach it.",
			Rows: []SettingSpec{
				{Name: "hostname", Label: "Server name", Kind: KindText, Min: 0, Max: 200,
					Hint: "Shown in the server browser and to connected players — live immediately."},
				{Name: "sv_lan", Label: "LAN only", Kind: KindBool,
					Hint: "Only local clients can connect. The internet cannot reach the server while set — needs a restart to take effect."},
			},
		},
	}
}

// allSettingSpecsCache spares every request a walk of the catalog.
var allSettingSpecsCache = AllSettingSpecs()

// AllSettingSpecs flattens the catalog (validation + form parsing).
func AllSettingSpecs() []SettingSpec {
	var out []SettingSpec
	for _, g := range SettingsCatalog() {
		out = append(out, g.Rows...)
	}
	return out
}

// SpecByName finds a catalog entry (nil when the cvar is not curated).
func SpecByName(name string) *SettingSpec {
	for i := range allSettingSpecsCache {
		if allSettingSpecsCache[i].Name == name {
			return &allSettingSpecsCache[i]
		}
	}
	return nil
}

// ValidateSettingValue enforces the catalog's type and range for one field.
// It is the panel-side gate; the agent re-validates names and sizes, but only
// this knows the intended type, so "thirty" never reaches server.cfg.
func ValidateSettingValue(spec *SettingSpec, val string) error {
	switch spec.Kind {
	case KindBool:
		if val != "0" && val != "1" {
			return errors.New("must be 0 or 1")
		}
	case KindInt:
		n, err := strconv.Atoi(val)
		if err != nil {
			return errors.New("must be a whole number")
		}
		if float64(n) < spec.Min || float64(n) > spec.Max {
			return fmt.Errorf("must be between %d and %d", int(spec.Min), int(spec.Max))
		}
	case KindFloat:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return errors.New("must be a number")
		}
		if f < spec.Min || f > spec.Max {
			return fmt.Errorf("must be between %g and %g", spec.Min, spec.Max)
		}
	case KindSelect:
		for _, o := range spec.Options {
			if o.Value == val {
				return nil
			}
		}
		return errors.New("not one of the allowed values")
	case KindText:
		if len(val) > int(spec.Max) {
			return fmt.Errorf("too long (max %d)", int(spec.Max))
		}
		if strings.ContainsAny(val, "\n\r\t\"") {
			return errors.New("quotes and control characters are not allowed")
		}
	}
	return nil
}
