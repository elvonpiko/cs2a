package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/go-sql-driver/mysql" // registered as "mysql", used only when wp_dsn is set
)

// LoadoutStore persists player cosmetic selections in the agent database and
// optionally syncs them into cs2-WeaponPaints' MySQL tables (which the plugin
// reads on player connect; players refresh in-game with !wp).
type LoadoutStore struct {
	cfg   Config
	store *Store
	wp    *sql.DB // nil unless wp_dsn configured
}

// NewLoadoutStore opens the loadout service, testing the MySQL connection
// lazily (nil wp means "no sync", everything still persists locally).
func NewLoadoutStore(cfg Config, store *Store) *LoadoutStore {
	ls := &LoadoutStore{cfg: cfg, store: store}
	if cfg.WPDsn != "" {
		if db, err := sql.Open("mysql", cfg.WPDsn); err == nil {
			db.SetConnMaxLifetime(5 * time.Minute)
			db.SetMaxOpenConns(2)
			ls.wp = db
		}
	}
	return ls
}

// Close releases the MySQL handle if any.
func (l *LoadoutStore) Close() {
	if l.wp != nil {
		l.wp.Close()
	}
}

// Loadout is a player's cosmetic selection. Knife values are WeaponPaints
// model names ("weapon_knife_karambit"); gloves are defindexes as strings.
type Loadout struct {
	KnifeT   string `json:"knife_t"`
	KnifeCT  string `json:"knife_ct"`
	GlovesT  string `json:"gloves_t,omitempty"`
	GlovesCT string `json:"gloves_ct,omitempty"`
	AgentT   string `json:"agent_t,omitempty"`
	AgentCT  string `json:"agent_ct,omitempty"`
}

// WPEnabled reports whether WeaponPaints MySQL sync is active.
func (l *LoadoutStore) WPEnabled() bool { return l.wp != nil }

// Get returns the stored loadout or a zero one.
func (l *LoadoutStore) Get(steamid string) (Loadout, error) {
	var raw string
	err := l.store.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, "loadout:"+steamid).Scan(&raw)
	if err == sql.ErrNoRows {
		return Loadout{}, nil
	}
	if err != nil {
		return Loadout{}, err
	}
	var lo Loadout
	if err := json.Unmarshal([]byte(raw), &lo); err != nil {
		return Loadout{}, fmt.Errorf("loadout: stored json: %w", err)
	}
	return lo, nil
}

// Set persists and best-effort syncs to WeaponPaints.
func (l *LoadoutStore) Set(steamid string, lo Loadout) error {
	if steamid == "" {
		return fmt.Errorf("loadout: empty steamid")
	}
	b, err := json.Marshal(lo)
	if err != nil {
		return err
	}
	if err := l.store.SetMeta("loadout:"+steamid, string(b)); err != nil {
		return err
	}
	if l.wp != nil {
		if err := l.syncWP(steamid, lo); err != nil {
			return fmt.Errorf("loadout: weaponpaints sync: %w", err)
		}
	}
	return nil
}

// syncWP writes wp_player_knife / wp_player_gloves / wp_player_agents rows.
// Table/column names verified against cs2-WeaponPaints source (weapon_team:
// 2=T, 3=CT; knife column holds the model name string).
func (l *LoadoutStore) syncWP(steamid string, lo Loadout) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	for team, knife := range map[int]string{2: lo.KnifeT, 3: lo.KnifeCT} {
		if knife == "" || knife == "default" {
			continue
		}
		if _, err := l.wp.ExecContext(ctx,
			`INSERT INTO wp_player_knife (steamid, weapon_team, knife) VALUES (?, ?, ?)
			 ON DUPLICATE KEY UPDATE knife = VALUES(knife)`,
			steamid, team, knife); err != nil {
			return err
		}
	}
	for team, glove := range map[int]string{2: lo.GlovesT, 3: lo.GlovesCT} {
		if glove == "" {
			continue
		}
		if _, err := l.wp.ExecContext(ctx,
			`INSERT INTO wp_player_gloves (steamid, weapon_team, weapon_defindex) VALUES (?, ?, ?)
			 ON DUPLICATE KEY UPDATE weapon_defindex = VALUES(weapon_defindex)`,
			steamid, team, glove); err != nil {
			return err
		}
	}
	if lo.AgentT != "" || lo.AgentCT != "" {
		if _, err := l.wp.ExecContext(ctx,
			`INSERT INTO wp_player_agents (steamid, agent_ct, agent_t) VALUES (?, ?, ?)
			 ON DUPLICATE KEY UPDATE agent_ct = VALUES(agent_ct), agent_t = VALUES(agent_t)`,
			steamid, lo.AgentCT, lo.AgentT); err != nil {
			return err
		}
	}
	return nil
}

// --- plugin config files ------------------------------------------------

// PluginConfigPath resolves the config JSON path for a catalog entry.
func (in *Installer) PluginConfigPath(id string) (string, error) {
	entry, ok := Find(in.catalog, id)
	if !ok {
		return "", fmt.Errorf("plugins: unknown catalog entry %q", id)
	}
	if entry.ConfigPath == "" {
		return "", fmt.Errorf("plugins: %s has no editable config", id)
	}
	return filepath.Join(in.cfg.CSGODir(), "addons", "counterstrikesharp", "configs", filepath.FromSlash(entry.ConfigPath)), nil
}

// GetPluginConfig returns the raw JSON of an entry's config file.
func (in *Installer) GetPluginConfig(id string) (raw []byte, exists bool, err error) {
	path, err := in.PluginConfigPath(id)
	if err != nil {
		return nil, false, err
	}
	raw, err = os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// PutPluginConfig validates and atomically writes an entry's config JSON.
func (in *Installer) PutPluginConfig(id string, raw []byte) error {
	path, err := in.PluginConfigPath(id)
	if err != nil {
		return err
	}
	// must be a JSON object; preserve nothing else
	dec := json.NewDecoder(bytes.NewReader(raw))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("plugins: config is not a JSON object: %w", err)
	}
	if err := ensureDir(dirOf(path)); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	return atomicWrite(path, buf.Bytes(), 0o644)
}
