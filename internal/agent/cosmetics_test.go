package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// The embedded catalogs must actually decode. The struct tags once named
// cs2-WeaponPaints' upstream column names (weapon_defindex/paint_name), which
// do not appear in data/gloves.json at all: every entry decoded as defindex 0
// with an empty name, so the loadout page offered 95 identical "Default (no
// gloves)" tiles and 65 nameless agents. A silent zero value is exactly what a
// test has to catch, because nothing else fails.
func TestGlovesDecodeWithNamesAndDefindexes(t *testing.T) {
	gloves := Gloves()
	if len(gloves) < 50 {
		t.Fatalf("glove catalog is suspiciously small: %d", len(gloves))
	}
	if gloves[0].Defindex != 0 || gloves[0].Name != "Default (no gloves)" {
		t.Errorf("first entry must be the default: %+v", gloves[0])
	}
	var defaults int
	for _, g := range gloves {
		if g.Defindex == 0 {
			defaults++
			continue
		}
		if g.Name == "" {
			t.Errorf("glove defindex %d has no name", g.Defindex)
		}
		if g.Paint == 0 {
			t.Errorf("glove %q (defindex %d) has no paint kit", g.Name, g.Defindex)
		}
		if !strings.HasPrefix(g.Image, "/static/img/gloves/") {
			t.Errorf("glove %q has an unusable image path %q", g.Name, g.Image)
		}
	}
	if defaults != 1 {
		t.Errorf("want exactly one default glove option, got %d", defaults)
	}
}

func TestAgentsDecodePerTeamWithNames(t *testing.T) {
	tSide, ctSide := Agents()
	if len(tSide) < 10 || len(ctSide) < 10 {
		t.Fatalf("agent catalog is suspiciously small: T=%d CT=%d", len(tSide), len(ctSide))
	}
	for side, list := range map[string][]AgentEntry{"T": tSide, "CT": ctSide} {
		if list[0].Model != "" || list[0].Name != "Default (no agent)" {
			t.Errorf("%s: first entry must be the default: %+v", side, list[0])
		}
		var defaults int
		for _, a := range list {
			if a.Model == "" {
				defaults++
				// The default is a "no agent" choice, so it must not carry an
				// image that would render as a real skin.
				if a.Image != "" {
					t.Errorf("%s: default agent must have no image, got %q", side, a.Image)
				}
				continue
			}
			if a.Name == "" {
				t.Errorf("%s: agent model %q has no name", side, a.Model)
			}
			if !strings.HasPrefix(a.Image, "/static/img/agents/") {
				t.Errorf("%s: agent %q has an unusable image path %q", side, a.Name, a.Image)
			}
		}
		if defaults != 1 {
			t.Errorf("%s: want exactly one default agent, got %d", side, defaults)
		}
	}
	// The two sides are distinct model sets: a CT model on the T list would put
	// an unloadable model in WeaponPaints' agent_t column.
	ctModels := map[string]bool{}
	for _, a := range ctSide {
		ctModels[a.Model] = true
	}
	for _, a := range tSide {
		if a.Model != "" && ctModels[a.Model] {
			t.Errorf("model %q appears on both sides", a.Model)
		}
	}
}

// Upstream's own spelling must keep working: the catalog files are refreshed
// from cs2-WeaponPaints, which names the fields weapon_defindex/paint_name.
func TestCosmeticsAcceptUpstreamFieldNames(t *testing.T) {
	var g []rawGlove
	if err := json.Unmarshal([]byte(`[{"weapon_defindex":5032,"paint":10010,"paint_name":"Hand Wraps"}]`), &g); err != nil {
		t.Fatal(err)
	}
	if g[0].defindex() != 5032 || g[0].label() != "Hand Wraps" {
		t.Errorf("upstream glove spelling not accepted: %+v", g[0])
	}
	var a []rawAgent
	if err := json.Unmarshal([]byte(`[{"team":2,"model":"m","agent_name":"Someone"}]`), &a); err != nil {
		t.Fatal(err)
	}
	if a[0].label() != "Someone" {
		t.Errorf("upstream agent spelling not accepted: %+v", a[0])
	}
}
