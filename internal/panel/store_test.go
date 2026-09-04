package panel

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUserLifecycle(t *testing.T) {
	s := openTestStore(t)

	u, err := s.CreateUser("Admin", "hash1", "admin", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.ID == 0 || u.Role != "admin" {
		t.Fatalf("user = %+v", u)
	}

	// duplicate username (case-insensitive) rejected
	if _, err := s.CreateUser("admin", "x", "player", ""); err == nil {
		t.Fatal("duplicate username accepted")
	}

	got, err := s.GetUserByUsername("ADMIN") // case-insensitive lookup
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Username != "Admin" || got.PasswordHash != "hash1" {
		t.Fatalf("got = %+v", got)
	}

	if _, err := s.GetUserByUsername("ghost"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	// steamid link + uniqueness
	if err := s.SetUserSteamID(u.ID, "76561197961500295"); err != nil {
		t.Fatal(err)
	}
	u2, _ := s.CreateUser("player2", "h", "player", "76561197961500296")
	if err := s.SetUserSteamID(u2.ID, "76561197961500295"); err == nil {
		t.Fatal("duplicate steamid accepted")
	}

	// list
	users, err := s.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("users = %d", len(users))
	}

	// delete
	if err := s.DeleteUser(u2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUserByID(u2.ID); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if err := s.DeleteUser(u2.ID); err != ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}
}

func TestSessions(t *testing.T) {
	s := openTestStore(t)
	u, _ := s.CreateUser("alice", "h", "admin", "")

	sess, err := s.CreateSession("tok123", u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ExpiresAt.Before(time.Now()) {
		t.Fatal("expiry wrong")
	}

	got, err := s.GetSessionUser("tok123")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Username != "alice" {
		t.Fatalf("user = %+v", got)
	}

	// expired session rejected
	if _, err := s.CreateSession("tok-old", u.ID, -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSessionUser("tok-old"); err != ErrNotFound {
		t.Fatalf("expired session should be ErrNotFound, got %v", err)
	}

	if err := s.DeleteSession("tok123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSessionUser("tok123"); err != ErrNotFound {
		t.Fatalf("deleted session: %v", err)
	}

	// gc
	if _, err := s.CreateSession("tok2", u.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteExpiredSessions(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSessionUser("tok2"); err != nil {
		t.Fatalf("valid session should survive gc: %v", err)
	}
}

func TestSettingsAndLoadouts(t *testing.T) {
	s := openTestStore(t)

	if v, _ := s.GetSetting("missing"); v != "" {
		t.Fatalf("want empty, got %q", v)
	}
	if err := s.SetSetting("k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("k", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetSetting("k"); v != "v2" {
		t.Fatalf("got %q", v)
	}

	if err := s.SetLoadout("76561197961500295", `{"knife":"weapon_knife_karambit"}`); err != nil {
		t.Fatal(err)
	}
	l, err := s.GetLoadout("76561197961500295")
	if err != nil {
		t.Fatal(err)
	}
	if l.Data != `{"knife":"weapon_knife_karambit"}` {
		t.Fatalf("loadout = %+v", l)
	}
	if err := s.SetLoadout("76561197961500295", `{"knife":"weapon_bayonet"}`); err != nil {
		t.Fatal(err)
	}
	l, _ = s.GetLoadout("76561197961500295")
	if l.Data != `{"knife":"weapon_bayonet"}` {
		t.Fatalf("upsert failed: %+v", l)
	}
	if _, err := s.GetLoadout("76561197960265729"); err != ErrNotFound {
		t.Fatalf("missing loadout: %v", err)
	}
}

func TestAudit(t *testing.T) {
	s := openTestStore(t)
	s.Audit("admin", "server.restart", "")
	s.Audit("admin", "plugin.install", "weaponpaints")
	entries, err := s.RecentAudit(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0]["action"] != "plugin.install" {
		t.Fatalf("newest first: %v", entries[0])
	}
}

func TestOpenStoreNested(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x", "y")
	s, err := OpenStore(filepath.Join(dir, "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(dir, "p.db")); err != nil {
		t.Fatal(err)
	}
}
