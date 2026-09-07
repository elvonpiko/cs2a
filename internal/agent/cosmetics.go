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

// rawGlove is one entry of data/gloves.json.
//
// The field tags must match the file that is actually embedded. They used to
// name cs2-WeaponPaints' upstream columns (weapon_defindex/paint_name), which
// appear nowhere in data/gloves.json: every glove therefore decoded with
// Defindex 0 and an empty name, hit the "defindex == 0" default branch, and the
// loadout page rendered 95 identical "Default (no gloves)" options. Both
// spellings are accepted so a catalog refreshed straight from upstream also
// decodes.
type rawGlove struct {
	Defindex       int    `json:"defindex"`
	WeaponDefindex int    `json:"weapon_defindex"`
	Paint          any    `json:"paint"`
	Name           string `json:"name"`
	PaintName      string `json:"paint_name"`
	Image          string `json:"image"`
}

// defindex returns whichever spelling the file used.
func (g rawGlove) defindex() int {
	if g.Defindex != 0 {
		return g.Defindex
	}
	return g.WeaponDefindex
}

// label returns whichever spelling the file used.
func (g rawGlove) label() string {
	if g.Name != "" {
		return g.Name
	}
	return g.PaintName
}

// rawAgent is one entry of data/agents.json. As with rawGlove, both the local
// and the upstream name spellings are accepted.
type rawAgent struct {
	Team      int    `json:"team"`
	Model     string `json:"model"`
	Name      string `json:"name"`
	AgentName string `json:"agent_name"`
	Image     string `json:"image"`
}

// label returns whichever spelling the file used.
func (a rawAgent) label() string {
	if a.Name != "" {
		return a.Name
	}
	return a.AgentName
}

// Gloves returns the glove catalog sorted by name (default first).
func Gloves() []GloveEntry {
	var raw []rawGlove
	if err := json.Unmarshal(glovesJSON, &raw); err != nil {
		return nil
	}
	out := make([]GloveEntry, 0, len(raw))
	seenDefault := false
	for _, g := range raw {
		paint := 0
		switch v := g.Paint.(type) {
		case float64:
			paint = int(v)
		case string:
			fmt.Sscanf(v, "%d", &paint)
		}
		// A glove selection is defindex + paint kit; the same defindex
		// appears with many paints — each paint is its own choice.
		if g.defindex() == 0 {
			// Exactly one "no gloves" option, whatever the data contains.
			if seenDefault {
				continue
			}
			seenDefault = true
			out = append(out, GloveEntry{Defindex: 0, Paint: 0, Name: "Default (no gloves)"})
			continue
		}
		out = append(out, GloveEntry{
			Defindex: g.defindex(),
			Paint:    paint,
			Name:     g.label(),
			Image:    gloveImage(g.Image),
		})
	}
	// Default first, then by name: an unnamed entry would otherwise sort to the
	// top and look like the default.
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Defindex == 0) != (out[j].Defindex == 0) {
			return out[i].Defindex == 0
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Agents returns the agent catalog split per team.
func Agents() (tSide, ctSide []AgentEntry) {
	var raw []rawAgent
	if err := json.Unmarshal(agentsJSON, &raw); err != nil {
		return nil, nil
	}
	for _, a := range raw {
		e := AgentEntry{Model: a.Model, Name: a.label(), Team: a.Team, Image: agentImage(a.Image)}
		if a.Model == "null" || a.Model == "" {
			e.Model = "" // default
			e.Name = "Default (no agent)"
			e.Image = ""
		}
		switch a.Team {
		case 2:
			tSide = append(tSide, e)
		case 3:
			ctSide = append(ctSide, e)
		}
	}
	sortAgents(tSide)
	sortAgents(ctSide)
	return tSide, ctSide
}

// sortAgents puts the default first and the rest in name order.
func sortAgents(list []AgentEntry) {
	sort.SliceStable(list, func(i, j int) bool {
		if (list[i].Model == "") != (list[j].Model == "") {
			return list[i].Model == ""
		}
		return list[i].Name < list[j].Name
	})
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

//go:embed data/weapons.json
var weaponsJSON []byte

// WeaponSkinEntry is one paint option for one weapon.
type WeaponSkinEntry struct {
	Paint int    `json:"paint"` // wp_player_skins.weapon_paint_id
	Name  string `json:"name"`
	Image string `json:"image,omitempty"` // Steam CDN URL
}

// WeaponEntry is one weapon with its selectable paint kits.
type WeaponEntry struct {
	Defindex int               `json:"defindex"` // wp_player_skins.weapon_defindex
	Name     string            `json:"name"`
	Team     string            `json:"team"` // "T", "CT" or "both"
	Skins    []WeaponSkinEntry `json:"skins"`
}

// weaponsCache avoids re-decoding the 400 KB catalog per request. The file is
// embedded; it never changes during a run.
var weaponsCache []WeaponEntry

// Weapons returns the gun-skin catalog: every regular weapon with the paint
// kits WeaponPaints can apply to it, in name order. Knives and gloves are not
// here — they have their own selectors (wp_player_knife / wp_player_gloves).
func Weapons() []WeaponEntry {
	if weaponsCache != nil {
		return weaponsCache
	}
	var raw []WeaponEntry
	if err := json.Unmarshal(weaponsJSON, &raw); err != nil {
		return nil
	}
	// defensive sort: catalog order is name-sorted at build time, but the
	// file is data, and a refreshed catalog should not shuffle the page.
	sort.SliceStable(raw, func(i, j int) bool { return raw[i].Name < raw[j].Name })
	for i := range raw {
		skins := raw[i].Skins
		sort.SliceStable(skins, func(a, b int) bool { return skins[a].Name < skins[b].Name })
	}
	weaponsCache = raw
	return raw
}
