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
// Every entry also carries Default: the value a standard competitive match
// runs (MR12 rules — the same values Premier/competitive matchmaking uses),
// so a fresh server starts with the rules players already know instead of
// "not set" placeholders. The panel falls back to Default whenever the
// managed block has no value for a cvar, which means:
//
//   - every field renders with the real value it will save,
//   - the admin can change one field and save — the rest silently keep the
//     standard they showed, instead of erroring one missing field at a time,
//   - "Reset to defaults" restores exactly this set.
//
// What is deliberately NOT here:
//   - sv_password: the Access page owns it (it is access control, and its
//     save path reloads the map so enforcement is immediate). The settings
//     save below preserves whatever the managed block holds for it.
//   - Cheat-protected, engine or client-only cvars: they are refused or
//     ignored by the server even when written to server.cfg.
//   - mp_warmup_start / mp_warmup_end: those are commands, not cvars — they
//     fire the moment they are read, so a line in server.cfg would start
//     warmup on every exec. The warmup *length* is a field below.

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
	// HasValue=false means the cvar has never been set through cs2a, and the
	// field then renders (and validates, and saves) Default.
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
	// Default is the standard competitive value (MR12) the field shows and
	// saves when the operator has not set anything else. Empty only for
	// cvars whose standard is "leave to the engine" (none today — every
	// curated row has one, so a save never trips on a missing field).
	Default string
}

type SettingOption struct {
	Value, Label string
}

func boolOption(v, l string) SettingOption { return SettingOption{v, l} }

// settingsCatalog is grouped in the order the page shows the cards.
//
// Default values are the MR12 competitive standard (the rules a player meets
// in Premier / competitive matchmaking): 24 max rounds, 1.92-minute rounds,
// 15 s freeze time, 40 s C4, $800 start money, $16000 cap, 45 s buy time,
// and friendly fire on.
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
				}, Default: "0",
					Hint: "Read at map load — saving changes game_mode with it, then reloads the map."},
				{Name: "game_mode", Label: "Game mode", Kind: KindSelect, Options: []SettingOption{
					boolOption("0", "Casual"),
					boolOption("1", "Competitive"),
					boolOption("2", "Wingman"),
					boolOption("3", "Arms Race"),
					boolOption("4", "Deathmatch"),
				}, Default: "1",
					Hint: "Classic (game_type 0) uses this. Saving reloads the map."},
			},
		},
		{
			Title: "Warmup",
			Blurb: "The free-for-all phase before the match starts. Length is a setting; start/end are commands the server runs by itself.",
			Rows: []SettingSpec{
				{Name: "mp_warmuptime", Label: "Warmup length (seconds)", Kind: KindInt, Min: 0, Max: 600, Default: "60",
					Hint: "Applies to the next warmup phase. 0 disables the warmup timer."},
				{Name: "mp_warmuptime_pausetimer", Label: "Warmup stays until players are ready", Kind: KindBool, Default: "0",
					Hint: "Extends warmup while set — live immediately when the server is running."},
				{Name: "mp_warmup_pausetimer", Label: "Pause the current warmup timer", Kind: KindBool, Default: "0",
					Hint: "Freezes the countdown — live immediately."},
			},
		},
		{
			Title: "Rounds",
			Blurb: "How long a round runs and how many rounds a match lasts.",
			Rows: []SettingSpec{
				{Name: "mp_freezetime", Label: "Freeze time (seconds)", Kind: KindInt, Min: 0, Max: 60, Default: "15",
					Hint: "Buy time at round start. Applies from the next round — live immediately."},
				{Name: "mp_roundtime", Label: "Round time (minutes)", Kind: KindInt, Min: 1, Max: 60, Default: "2",
					Hint: "Round length. CS2 shows this live once set. (Matchmaking runs 1.92 min = 1:55; the field takes whole minutes, so 2 is the closest standard.)"},
				{Name: "mp_maxrounds", Label: "Max rounds (0 = unlimited)", Kind: KindInt, Min: 0, Max: 100, Default: "24",
					Hint: "First to half+1 wins. Applies to the next match — the current one keeps its limit."},
				{Name: "mp_round_restart_delay", Label: "Round restart delay (seconds)", Kind: KindFloat, Min: 0, Max: 30, Default: "7",
					Hint: "Time between the round ending and the next starting — live immediately."},
			},
		},
		{
			Title: "Economy",
			Blurb: "Player money: what they start with, the cap, and the per-round bonus.",
			Rows: []SettingSpec{
				{Name: "mp_startmoney", Label: "Starting money", Kind: KindInt, Min: 0, Max: 16000, Default: "800",
					Hint: "Money at spawn/reset. Applies from the next round."},
				{Name: "mp_maxmoney", Label: "Maximum money", Kind: KindInt, Min: 0, Max: 16000, Default: "16000",
					Hint: "The cap a player can hold — live immediately."},
				{Name: "mp_afterroundmoney", Label: "Money after each round (0 = normal rules)", Kind: KindInt, Min: 0, Max: 16000, Default: "0",
					Hint: "Set above 0 to give every player this much each round end, win or lose."},
				{Name: "mp_buytime", Label: "Buy time (seconds)", Kind: KindInt, Min: 10, Max: 300, Default: "45",
					Hint: "How long the buy menu stays open. Applies from the next round."},
			},
		},
		{
			Title: "Bomb",
			Blurb: "C4 behaviour: the timer and who carries it.",
			Rows: []SettingSpec{
				{Name: "mp_c4timer", Label: "C4 timer (seconds)", Kind: KindInt, Min: 10, Max: 300, Default: "40",
					Hint: "Time from planting to detonation — live immediately."},
				{Name: "mp_give_player_c4", Label: "Give the C4 to a player", Kind: KindBool, Default: "1",
					Hint: "Off = the bomb stays on the ground at round start. Applies from the next round."},
			},
		},
		{
			Title: "Teams",
			Blurb: "Team balance and size limits.",
			Rows: []SettingSpec{
				{Name: "mp_autoteambalance", Label: "Auto-balance teams", Kind: KindBool, Default: "0",
					Hint: "Moves players between rounds when one side is stacked. Applies from the next round. (Off in competitive play — teams swap at halftime instead.)"},
				{Name: "mp_limitteams", Label: "Max team size difference (0 = no limit)", Kind: KindInt, Min: 0, Max: 8, Default: "1",
					Hint: "Blocks joining the bigger team beyond this — live immediately."},
			},
		},
		{
			Title: "Players",
			Blurb: "Friendly fire, damage scaling and gravity.",
			Rows: []SettingSpec{
				{Name: "mp_friendlyfire", Label: "Friendly fire", Kind: KindBool, Default: "1",
					Hint: "Teammates can damage each other — live immediately. (On in competitive play.)"},
				{Name: "mp_damage_scale_ct_head", Label: "CT head damage scale", Kind: KindFloat, Min: 0, Max: 10, Default: "1",
					Hint: "1 = normal. Halve it for tanky CTs — live immediately."},
				{Name: "mp_damage_scale_ct_body", Label: "CT body damage scale", Kind: KindFloat, Min: 0, Max: 10, Default: "1",
					Hint: "1 = normal — live immediately."},
				{Name: "mp_damage_scale_t_head", Label: "T head damage scale", Kind: KindFloat, Min: 0, Max: 10, Default: "1",
					Hint: "1 = normal — live immediately."},
				{Name: "mp_damage_scale_t_body", Label: "T body damage scale", Kind: KindFloat, Min: 0, Max: 10, Default: "1",
					Hint: "1 = normal — live immediately."},
				{Name: "sv_gravity", Label: "Gravity (800 = normal)", Kind: KindFloat, Min: 100, Max: 2000, Default: "800",
					Hint: "Lower = floatier jumps — live immediately."},
			},
		},
		{
			Title: "Server",
			Blurb: "How the server names itself and who can reach it.",
			Rows: []SettingSpec{
				{Name: "hostname", Label: "Server name", Kind: KindText, Min: 0, Max: 200, Default: "cs2a server",
					Hint: "Shown in the server browser and to connected players — live immediately."},
				{Name: "sv_lan", Label: "LAN only", Kind: KindBool, Default: "0",
					Hint: "Only local clients can connect. The internet cannot reach the server while set — needs a restart to take effect."},
			},
		},
	}
}

// EffectiveValue returns what the field shows and saves: the operator's value
// when one is set, the standard default otherwise.
func (s SettingSpec) EffectiveValue() string {
	if s.HasValue && s.Value != "" {
		return s.Value
	}
	return s.Default
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
