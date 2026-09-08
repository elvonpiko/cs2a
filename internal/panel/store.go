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
	// touched records sessions whose sliding window was just refreshed, so
	// the middleware can re-issue the matching cookie once, in the same
	// response that refreshed the row. It is a flag, not a timestamp: the
	// decision was already made inside GetSessionUser.
	touched map[string]bool
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
	s := &Store{db: db, touched: map[string]bool{}}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// Expired sessions are rejected on every request, but nothing ever removed
	// the rows: a panel left running for months accumulated one row per login
	// forever, in a database the operator has no UI to prune. Sweeping at open
	// (and after each login) keeps the table bounded without a goroutine.
	_ = s.DeleteExpiredSessions()
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
	// Sliding sessions need last_seen; deployments from before it have a
	// sessions table without the column. SQLite has no ADD COLUMN IF NOT
	// EXISTS, so check pragma table_info first.
	if !s.columnExists("sessions", "last_seen") {
		if _, err := s.db.Exec(`ALTER TABLE sessions ADD COLUMN last_seen TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("panel: migrate sessions.last_seen: %w", err)
		}
	}
	return nil
}

// columnExists reports whether a table has a column.
func (s *Store) columnExists(table, column string) bool {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
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

// SetUserRole changes a user's role. Demoting the last admin would lock
// everyone out of admin-only pages, so it is refused here — the panel has no
// recovery path for a zero-admin install short of the setup token.
func (s *Store) SetUserRole(id int64, role string) error {
	if role != "admin" && role != "player" {
		return fmt.Errorf("panel: bad role %q", role)
	}
	if role == "player" {
		var admins int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND id != ?`, id).Scan(&admins); err != nil {
			return err
		}
		if admins == 0 {
			return fmt.Errorf("panel: demote would leave no admins")
		}
	}
	res, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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
// last_seen starts at creation, so the idle clock begins the moment the
// browser is closed, not at the first later request.
func (s *Store) CreateSession(tokenHash string, userID int64, ttl time.Duration) (*Session, error) {
	now := time.Now().UTC()
	exp := now.Add(ttl)
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, expires_at, created_at, last_seen)
		VALUES (?, ?, ?, ?, ?)`, tokenHash, userID, exp.Format(time.RFC3339), now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	return &Session{TokenHash: tokenHash, UserID: userID, ExpiresAt: exp, CreatedAt: now}, nil
}

// GetSessionUser resolves a session token to its user, if valid.
//
// Two expiry clocks run at once: the absolute cap (expires_at, set at login)
// and the sliding idle window (last_seen + SessionIdle). A row is valid only
// inside both. This query returning nothing is the signal for "expired" — the
// caller cannot distinguish idle from absolute, which is fine: the login page
// explains the rule rather than the reason.
func (s *Store) GetSessionUser(tokenHash string) (*User, error) {
	now := time.Now().UTC()
	nowS := now.Format(time.RFC3339)
	idleBefore := now.Add(-SessionIdle).Format(time.RFC3339)
	row := s.db.QueryRow(`SELECT u.id, u.username, u.password_hash, u.role, COALESCE(u.steamid64,''), u.created_at,
			s.expires_at, COALESCE(s.last_seen, s.created_at)
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?
		  AND s.expires_at > ?
		  AND COALESCE(s.last_seen, s.created_at) > ?`,
		tokenHash, nowS, idleBefore)
	var exp, seen, created string
	u := &User{}
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.SteamID64, &created, &exp, &seen)
	if err != nil {
		return nil, ErrNotFound
	}
	if t, err := time.Parse(time.RFC3339, created); err == nil {
		u.CreatedAt = t
	}
	// Sliding window: refresh last_seen (and the cookie's Max-Age with it) at
	// most once per SessionTouch so a polling page is not a write per poll.
	if t, err := time.Parse(time.RFC3339, seen); err == nil {
		if now.Sub(t) >= SessionTouch {
			_, _ = s.db.Exec(`UPDATE sessions SET last_seen = ? WHERE token_hash = ?`, nowS, tokenHash)
			s.touched[tokenHash] = true
		}
	}
	return u, nil
}

// SessionTouched reports whether the previous GetSessionUser call for this
// token refreshed the sliding window (and clears the flag). The middleware
// uses it to re-issue the cookie in the same response.
func (s *Store) SessionTouched(tokenHash string) bool {
	if !s.touched[tokenHash] {
		return false
	}
	delete(s.touched, tokenHash)
	return true
}

// DeleteSession removes a session (logout).
func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteExpiredSessions garbage-collects dead sessions: past the absolute cap,
// or idle for longer than the sliding window (a browser closed days ago).
func (s *Store) DeleteExpiredSessions() error {
	now := time.Now().UTC()
	idleBefore := now.Add(-SessionIdle).Format(time.RFC3339)
	_, err := s.db.Exec(`DELETE FROM sessions
		WHERE expires_at <= ?
		   OR COALESCE(last_seen, created_at) <= ?`, now.Format(time.RFC3339), idleBefore)
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
