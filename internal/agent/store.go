package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, registered as "sqlite"
)

// Store is the agent's persistent state (installed plugins, misc metadata).
type Store struct {
	db *sql.DB
}

// PluginState records one installed plugin/component.
type PluginState struct {
	Name        string            `json:"name"`     // catalog id, e.g. "metamod"
	Version     string            `json:"version"`  // resolved release version at install time
	Status      string            `json:"status"`   // "installed" | "failed"
	Manifest    map[string]string `json:"manifest"` // artifacts created, for uninstall
	InstalledAt time.Time         `json:"installed_at"`
}

// OpenStore opens (creating if needed) the agent database and runs migrations.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("agent: mkdir db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("agent: open db: %w", err)
	}
	// sqlite defaults can block on concurrent access; cs2a is single-writer
	// but be forgiving anyway.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("agent: pragma %q: %w", pragma, err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS plugin_state (
			name         TEXT PRIMARY KEY,
			version      TEXT NOT NULL DEFAULT '',
			status       TEXT NOT NULL DEFAULT 'installed',
			manifest     TEXT NOT NULL DEFAULT '{}',
			installed_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("agent: migrate: %w", err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

var ErrNotFound = errors.New("agent: not found")

// SetPluginState inserts or updates a plugin record.
func (s *Store) SetPluginState(p PluginState) error {
	if p.InstalledAt.IsZero() {
		p.InstalledAt = time.Now().UTC()
	}
	man, err := jsonString(p.Manifest)
	if err != nil {
		return fmt.Errorf("agent: marshal manifest: %w", err)
	}
	_, err = s.db.Exec(`INSERT INTO plugin_state (name, version, status, manifest, installed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET version=excluded.version, status=excluded.status,
			manifest=excluded.manifest, installed_at=excluded.installed_at`,
		p.Name, p.Version, p.Status, man, p.InstalledAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("agent: set plugin state: %w", err)
	}
	return nil
}

// GetPluginState fetches one plugin record.
func (s *Store) GetPluginState(name string) (PluginState, error) {
	row := s.db.QueryRow(`SELECT name, version, status, manifest, installed_at FROM plugin_state WHERE name = ?`, name)
	return scanPluginState(row.Scan)
}

// ListPluginStates returns all plugin records ordered by name.
func (s *Store) ListPluginStates() ([]PluginState, error) {
	rows, err := s.db.Query(`SELECT name, version, status, manifest, installed_at FROM plugin_state ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("agent: list plugin states: %w", err)
	}
	defer rows.Close()
	var out []PluginState
	for rows.Next() {
		p, err := scanPluginState(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePluginState removes a plugin record.
func (s *Store) DeletePluginState(name string) error {
	res, err := s.db.Exec(`DELETE FROM plugin_state WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("agent: delete plugin state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMeta stores a metadata key.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("agent: set meta: %w", err)
	}
	return nil
}

// GetMeta reads a metadata key.
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("agent: get meta: %w", err)
	}
	return v, nil
}

// jsonString marshals m, returning "{}" for nil maps.
func jsonString(m map[string]string) (string, error) {
	if m == nil {
		m = map[string]string{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func scanPluginState(scan func(dest ...any) error) (PluginState, error) {
	var p PluginState
	var man, at string
	if err := scan(&p.Name, &p.Version, &p.Status, &man, &at); err != nil {
		if errorsIs(err) {
			return p, ErrNotFound
		}
		return p, fmt.Errorf("agent: scan plugin state: %w", err)
	}
	if err := json.Unmarshal([]byte(man), &p.Manifest); err != nil {
		return p, fmt.Errorf("agent: parse manifest: %w", err)
	}
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		p.InstalledAt = t
	}
	return p, nil
}

// errorsIs reports whether err is sql.ErrNoRows without importing
// database/sql at every call site.
func errorsIs(err error) bool {
	return err == sql.ErrNoRows
}
