// Package panel implements the cs2a control plane: an SSR web UI (templ +
// htmx) for admins and players, backed by SQLite, driving the agent over its
// loopback API.
package panel

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the panel's persistent state.
type Store struct {
	db *sql.DB
}

// User is a panel account. Roles: "admin" | "player".
// PasswordHash is populated only by auth queries and never leaves the server.
type User struct {
	ID           int64
	Username     string
	Role         string
	SteamID64    string
	PasswordHash string `json:"-"`
	CreatedAt    time.Time
}

// Session identifies a logged-in browser session (token itself is stored
// hashed).
type Session struct {
	TokenHash string
	UserID    int64
	ExpiresAt time.Time
	CreatedAt time.Time
}

// OpenStore opens/creates the panel database and runs migrations.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("panel: mkdir db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("panel: open db: %w", err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("panel: pragma: %w", err)
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
		`CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'player',
			steamid64     TEXT UNIQUE,
			created_at    TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY,
			user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS loadouts (
			steamid64  TEXT PRIMARY KEY,
			data       TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS audit (
			id       INTEGER PRIMARY KEY AUTOINCREMENT,
			at       TEXT NOT NULL,
			username TEXT NOT NULL DEFAULT '',
			action   TEXT NOT NULL,
			detail   TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_at ON audit(at)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("panel: migrate: %w", err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("panel: not found")

// CreateUser inserts a user; username must be unique (case-insensitive).
func (s *Store) CreateUser(username, passwordHash, role, steamid string) (*User, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.Exec(`INSERT INTO users (username, password_hash, role, steamid64, created_at)
		VALUES (?, ?, ?, ?, ?)`, username, passwordHash, role, nullIfEmpty(steamid), now)
	if err != nil {
		return nil, fmt.Errorf("panel: create user: %w", err)
	}
	id, _ := res.LastInsertId()
	return &User{ID: id, Username: username, Role: role, SteamID64: steamid, CreatedAt: time.Now().UTC()}, nil
}

// GetUserByUsername fetches a user for login.
func (s *Store) GetUserByUsername(username string) (*User, error) {
	row := s.db.QueryRow(`SELECT id, username, password_hash, role, COALESCE(steamid64,''), created_at
		FROM users WHERE username = ?`, username)
	return scanUser(row.Scan)
}

// GetUserByID fetches a user by id.
func (s *Store) GetUserByID(id int64) (*User, error) {
	row := s.db.QueryRow(`SELECT id, username, password_hash, role, COALESCE(steamid64,''), created_at
		FROM users WHERE id = ?`, id)
	return scanUser(row.Scan)
}

// ListUsers returns all users ordered by name.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, username, password_hash, role, COALESCE(steamid64,''), created_at
		FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// SetUserSteamID links (or changes) a user's SteamID64.
func (s *Store) SetUserSteamID(id int64, steamid string) error {
	_, err := s.db.Exec(`UPDATE users SET steamid64 = ? WHERE id = ?`, nullIfEmpty(steamid), id)
	return err
}

// SetUserPassword updates a user's password hash.
func (s *Store) SetUserPassword(id int64, hash string) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	return err
}

// DeleteUser removes a user (sessions cascade).
func (s *Store) DeleteUser(id int64) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanUser(scan func(dest ...any) error) (*User, error) {
	var u User
	var created string
	if err := scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.SteamID64, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339, created); err == nil {
		u.CreatedAt = t
	}
	return &u, nil
}

// --- sessions -----------------------------------------------------------

// CreateSession stores a new session for a user (token given hashed).
func (s *Store) CreateSession(tokenHash string, userID int64, ttl time.Duration) (*Session, error) {
	now := time.Now().UTC()
	exp := now.Add(ttl)
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, expires_at, created_at)
		VALUES (?, ?, ?, ?)`, tokenHash, userID, exp.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	return &Session{TokenHash: tokenHash, UserID: userID, ExpiresAt: exp, CreatedAt: now}, nil
}

// GetSessionUser resolves a session token to its user, if valid.
func (s *Store) GetSessionUser(tokenHash string) (*User, error) {
	row := s.db.QueryRow(`SELECT u.id, u.username, u.password_hash, u.role, COALESCE(u.steamid64,''), u.created_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, tokenHash, time.Now().UTC().Format(time.RFC3339))
	return scanUser(row.Scan)
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteExpiredSessions garbage-collects old sessions.
func (s *Store) DeleteExpiredSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, time.Now().UTC().Format(time.RFC3339))
	return err
}

// --- settings -----------------------------------------------------------

// GetSetting returns a settings value or "".
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// SetSetting upserts a settings value.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// --- loadouts -----------------------------------------------------------

// Loadout is a player's cosmetics selection (opaque JSON blob managed by
// panel/agent clients).
type Loadout struct {
	SteamID64 string
	Data      string
	UpdatedAt time.Time
}

// SetLoadout upserts a player's loadout JSON.
func (s *Store) SetLoadout(steamid, data string) error {
	_, err := s.db.Exec(`INSERT INTO loadouts (steamid64, data, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(steamid64) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`,
		steamid, data, time.Now().UTC().Format(time.RFC3339))
	return err
}

// GetLoadout fetches a player's loadout.
func (s *Store) GetLoadout(steamid string) (*Loadout, error) {
	var l Loadout
	var at string
	err := s.db.QueryRow(`SELECT steamid64, data, updated_at FROM loadouts WHERE steamid64 = ?`, steamid).
		Scan(&l.SteamID64, &l.Data, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339, at); err == nil {
		l.UpdatedAt = t
	}
	return &l, nil
}

// --- audit --------------------------------------------------------------

// Audit appends an audit entry.
func (s *Store) Audit(username, action, detail string) {
	_, _ = s.db.Exec(`INSERT INTO audit (at, username, action, detail) VALUES (?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), username, action, detail)
}

// RecentAudit returns the last n audit entries (newest first).
func (s *Store) RecentAudit(n int) ([]map[string]string, error) {
	rows, err := s.db.Query(`SELECT at, username, action, detail FROM audit ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var at, user, action, detail string
		if err := rows.Scan(&at, &user, &action, &detail); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"at": at, "user": user, "action": action, "detail": detail})
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
