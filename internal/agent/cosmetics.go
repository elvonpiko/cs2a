package agent

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Cosmetic catalogs for the panel loadout page. Values match exactly what
// cs2-WeaponPaints writes/read:
//   - knives: model name in wp_player_knife.knife (team 2/3)
//   - gloves: GLOVE defindex in wp_player_gloves.weapon_defindex, paint kit
//     id in wp_player_skins (weapon_defindex = glove defindex, paint_id)
//   - agents: model path in wp_player_agents.agent_t / agent_ct (single row)

//go:embed data/gloves.json
var glovesJSON []byte

//go:embed data/agents.json
var agentsJSON []byte

// GloveEntry is one selectable glove pair.
type GloveEntry struct {
	Defindex int    `json:"defindex"` // wp_player_gloves.weapon_defindex
	Paint    int    `json:"paint"`    // paint kit id for wp_player_skins
	Name     string `json:"name"`
	Image    string `json:"image,omitempty"` // static path under /static/img/gloves
}

// AgentEntry is one selectable agent model.
type AgentEntry struct {
	Model string `json:"model"` // wp_player_agents.agent_t / agent_ct value
	Name  string `json:"name"`
	Team  int    `json:"team"` // 2 = T, 3 = CT (0 = any, filtered client-side)
	Image string `json:"image,omitempty"`
}

// glovePaintLookup: defindex -> paint for the default paint of each glove model.
// The plugin stores glove defindex in wp_player_gloves and its paint kit in
// wp_player_skins; the panel needs both to render + write a selection.
var glovePaintLookup = map[int]int{}

type rawGlove struct {
	WeaponDefindex int    `json:"weapon_defindex"`
	Paint          any    `json:"paint"`
	PaintName      string `json:"paint_name"`
	Image          string `json:"image"`
}

type rawAgent struct {
	Team      int    `json:"team"`
	Model     string `json:"model"`
	AgentName string `json:"agent_name"`
	Image     string `json:"image"`
}

// Gloves returns the glove catalog sorted by name (default first).
func Gloves() []GloveEntry {
	var raw []rawGlove
	if err := json.Unmarshal(glovesJSON, &raw); err != nil {
		return nil
	}
	out := make([]GloveEntry, 0, len(raw))
	for _, g := range raw {
		paint := 0
		switch v := g.Paint.(type) {
		case float64:
			paint = int(v)
		case string:
			fmt.Sscanf(v, "%d", &paint)
		}
		// A glove selection is defindex + paint kit; the same defindex
		// appears with many paints — keep the default/first paint per pair.
		if g.WeaponDefindex == 0 {
			out = append(out, GloveEntry{Defindex: 0, Paint: 0, Name: "Default (no gloves)"})
			continue
		}
		out = append(out, GloveEntry{
			Defindex: g.WeaponDefindex,
			Paint:    paint,
			Name:     g.PaintName,
			Image:    gloveImage(g.Image),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Agents returns the agent catalog split per team.
func Agents() (tSide, ctSide []AgentEntry) {
	var raw []rawAgent
	if err := json.Unmarshal(agentsJSON, &raw); err != nil {
		return nil, nil
	}
	for _, a := range raw {
		e := AgentEntry{Model: a.Model, Name: a.AgentName, Team: a.Team, Image: agentImage(a.Image)}
		if a.Model == "null" || a.Model == "" {
			e.Model = "" // default
			e.Name = "Default (no agent)"
		}
		switch a.Team {
		case 2:
			tSide = append(tSide, e)
		case 3:
			ctSide = append(ctSide, e)
		}
	}
	sort.Slice(tSide, func(i, j int) bool { return tSide[i].Name < tSide[j].Name })
	sort.Slice(ctSide, func(i, j int) bool { return ctSide[i].Name < ctSide[j].Name })
	return tSide, ctSide
}

// gloveImage converts the plugin's absolute image URL to a local static path
// when we ship the image; empty means no local image.
func gloveImage(abs string) string {
	name := abs[strings.LastIndexByte(abs, '/')+1:]
	if name == "" {
		return ""
	}
	return "/static/img/gloves/" + name
}

func agentImage(abs string) string {
	name := abs[strings.LastIndexByte(abs, '/')+1:]
	if name == "" {
		return ""
	}
	return "/static/img/agents/" + name
}
