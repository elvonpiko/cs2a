package panel

import (
	"context"
	"sync"
	"time"
)

// capsTTL bounds how long a nav render trusts a cached plugin answer. A
// plugin install or uninstall is rare, but the tab appearing or vanishing
// should follow one Plugins-page visit, not "whenever the panel restarts".
const capsTTL = 15 * time.Second

// capsCache memoizes "which feature-defining plugins are installed" per
// request burst. Every page render builds the nav, and the nav decides the
// Loadout tab from WeaponPaints' presence; without the cache that is one
// agent round-trip (and one plugin-state DB read on the agent) per render.
type capsCache struct {
	mu        sync.Mutex
	fetched   time.Time
	installed map[string]bool
}

// reset drops the cached answer. The panel calls it whenever it knows plugin
// state changed — its own install/uninstall actions — so the Loadout tab
// follows the operator's click immediately instead of one TTL later.
func (c *capsCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.installed = nil
}

// weaponPaints reports whether the WeaponPaints plugin is installed. A
// transport error counts as "no" for nav purposes — the tab hides until the
// agent answers — but the value is not cached in that case, so a recovered
// agent restores the tab on the next render.
func (c *capsCache) weaponPaints(ctx context.Context, agent *AgentClient) bool {
	if v, ok := c.get(); ok {
		return v["weaponpaints"]
	}
	plugins, err := agent.Plugins(ctx)
	if err != nil {
		return false
	}
	installed := map[string]bool{}
	for _, p := range plugins {
		installed[p.ID] = p.Installed
	}
	c.put(installed)
	return installed["weaponpaints"]
}

func (c *capsCache) get() (map[string]bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.installed == nil || time.Since(c.fetched) > capsTTL {
		return nil, false
	}
	return c.installed, true
}

func (c *capsCache) put(installed map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.installed = installed
	c.fetched = time.Now()
}
