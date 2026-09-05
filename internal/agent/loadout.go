package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// Loadout is a player's cosmetic selection.
//   - knives: WeaponPaints model names ("weapon_knife_karambit") → wp_player_knife.knife
//   - gloves: "<defindex>:<paint>" (e.g. "5032:10010") → defindex into
//     wp_player_gloves.weapon_defindex and the paint kit into wp_player_skins
//   - agents: model path ("ctm_st6/ctm_st6_variantj") → wp_player_agents.agent_t/agent_ct
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

// Set persists the selection, then syncs it into WeaponPaints' database.
//
// A sync failure is reported as a warning, not an error: the local write already
// succeeded, so answering "Save failed" would be wrong twice over — the panel
// would show the selection it had just stored as rejected, and the player would
// retry a save that cannot fix a database problem. The common causes are a
// WeaponPaints database that is unreachable and tables that do not exist yet
// (the plugin creates them the first time it loads), and both need an admin,
// not a retry.
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
			return warnf("saved, but the WeaponPaints database did not accept it: %v", wpSyncHint(err))
		}
	}
	return nil
}

// wpSyncHint turns the driver's error into something an admin can act on.
// "Error 1146 (42S02): Table 'cs2.wp_player_knife' doesn't exist" is accurate
// and useless; the tables are created by the plugin on its first successful
// load, so the fix is to start the server with WeaponPaints configured.
func wpSyncHint(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "1146") || strings.Contains(msg, "doesn't exist"):
		return "its tables do not exist yet — start the server once with WeaponPaints installed and its database credentials set, and it will create them"
	case strings.Contains(msg, "1045") || strings.Contains(msg, "Access denied"):
		return "the database rejected the credentials in wp_dsn"
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "dial tcp"):
		return "the database is not reachable from this server (check wp_dsn and that MariaDB is running)"
	default:
		return msg
	}
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
		if glove == "" || glove == "default" {
			continue
		}
		defindex, paint, ok := parseGlove(glove)
		if !ok {
			return fmt.Errorf("loadout: bad glove value %q (want \"<defindex>:<paint>\")", glove)
		}
		if _, err := l.wp.ExecContext(ctx,
			`INSERT INTO wp_player_gloves (steamid, weapon_team, weapon_defindex) VALUES (?, ?, ?)
			 ON DUPLICATE KEY UPDATE weapon_defindex = VALUES(weapon_defindex)`,
			steamid, team, defindex); err != nil {
			return err
		}
		// the paint kit lives in the skins table keyed by the glove defindex
		if paint != 0 {
			if _, err := l.wp.ExecContext(ctx,
				`INSERT INTO wp_player_skins (steamid, weapon_defindex, weapon_team, weapon_paint_id, weapon_wear, weapon_seed)
				 VALUES (?, ?, ?, ?, 0.000001, 0)
				 ON DUPLICATE KEY UPDATE weapon_paint_id = VALUES(weapon_paint_id)`,
				steamid, defindex, team, paint); err != nil {
				return err
			}
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

// parseGlove splits "<defindex>:<paint>".
func parseGlove(v string) (defindex, paint int64, ok bool) {
	def, paintStr, found := strings.Cut(v, ":")
	if !found {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n, 0, true
		}
		return 0, 0, false
	}
	defindex, err := strconv.ParseInt(def, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	paint, err = strconv.ParseInt(paintStr, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return defindex, paint, true
}

// --- plugin config files ------------------------------------------------

// PluginConfigPath resolves the config JSON path for a catalog entry.
// ConfigPath is relative to game/csgo (config locations differ by plugin type:
// cssharp plugins keep theirs under addons/counterstrikesharp/configs/plugins,
// metamod plugins under their own addons dir or cfg/).
func (in *Installer) PluginConfigPath(id string) (string, error) {
	entry, ok := Find(in.catalog, id)
	if !ok {
		return "", fmt.Errorf("plugins: unknown catalog entry %q", id)
	}
	if entry.ConfigPath == "" {
		return "", fmt.Errorf("plugins: %s has no editable config", id)
	}
	csgo := in.cfg.CSGODir()
	path := filepath.Join(csgo, filepath.FromSlash(entry.ConfigPath))
	if !safeSubPath(csgo, path) {
		return "", fmt.Errorf("plugins: %s has an unsafe config path", id)
	}
	return path, nil
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
	if err := atomicWrite(path, buf.Bytes(), 0o644); err != nil {
		return err
	}
	// atomicWrite replaces the file, so a config the game user owned would come
	// back owned by root (the agent) and the plugin could no longer update it.
	if err := in.alignAbsToGameOwner(dirOf(path), path); err != nil {
		return fmt.Errorf("plugins: config saved but not writable by the game user: %w", err)
	}
	return nil
}
