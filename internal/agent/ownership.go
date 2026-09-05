package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// The agent runs as root so it can drive systemd, but the game server runs as
// an unprivileged user (steam by default). Files the agent unpacks into the
// game tree therefore land root-owned, and CounterStrikeSharp cannot write the
// config files it generates on first load — plugins silently fail to configure
// themselves, or fail outright when a plugin needs a writable data directory.
//
// The fix needs no extra configuration: the csgo directory already belongs to
// whoever runs the game, so newly written paths inherit that owner.

// treeOwner reports the uid/gid that owns dir. ok is false when ownership
// cannot be determined: a missing directory, or a platform whose stat does not
// carry Unix ids. Symlinks are resolved, since the caller names a directory it
// wants the owner of, not the link pointing at it.
func treeOwner(dir string) (uid, gid int, ok bool) {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return statOwner(dir)
}

// alignOwnership makes every path under root (inclusive) owned by uid/gid,
// matching the game tree's owner. Errors are collected rather than fatal: a
// plugin whose files are readable but wrongly owned is still better than a
// failed install, and the caller reports the problem as a warning.
func alignOwnership(root string, uid, gid int) error {
	if os.Geteuid() != 0 {
		// Only root may chown to another user; as a non-root agent every file
		// is already created with the right owner.
		return nil
	}
	var errs []error
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A vanished file is not a problem worth failing an install for.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			errs = append(errs, err)
			return nil
		}
		// Lchown, not Chown: a symlink must not have its target's owner
		// rewritten (the archive extractor stores symlinks as regular files
		// today, but that is not something this helper should depend on).
		if err := os.Lchown(p, uid, gid); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		_ = d
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("chown %s: %w", root, errors.Join(errs...))
	}
	return nil
}

// gameOwner reports the uid/gid that files in the game tree must belong to.
//
// The csgo directory's own owner is the primary answer: it is what the game
// process already writes as. When that directory is root-owned the tree gives no
// useful signal, so the game unit's User= is resolved instead — a server whose
// content was unpacked by root but that runs as an unprivileged account is
// exactly the case where CounterStrikeSharp cannot write the configs it
// generates, and plugins then fail with nothing in the panel to explain it.
func (in *Installer) gameOwner() (uid, gid int, ok bool) {
	csgo := in.cfg.CSGODir()
	uid, gid, ok = treeOwner(csgo)
	if ok && !(uid == 0 && gid == 0) {
		return uid, gid, true
	}
	// Fall back to the account systemd starts the game as.
	name := in.unitUser()
	if name == "" || name == "root" {
		return 0, 0, false
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, false
	}
	nuid, err1 := strconv.Atoi(u.Uid)
	ngid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil || nuid == 0 {
		return 0, 0, false
	}
	return nuid, ngid, true
}

// unitUserReader is the part of a service controller that can name the account
// the game runs as. It is an interface so tests can supply a fake.
type unitUserReader interface {
	UnitUser() string
}

// unitUser asks systemd which user the configured game unit runs as.
func (in *Installer) unitUser() string {
	if in.sysd == nil {
		if in.cfg.ServiceName == "" {
			return ""
		}
		in.sysd = NewSystemd(in.cfg.ServiceName)
	}
	return in.sysd.UnitUser()
}

// applyGameOwnership aligns the paths an install created with the game tree's
// owner. It returns a human-readable warning instead of an error because a
// completed install with imperfect ownership must not be reported as a failure.
func (in *Installer) applyGameOwnership(paths []string) string {
	csgo := in.cfg.CSGODir()
	uid, gid, ok := in.gameOwner()
	if !ok {
		return ""
	}
	var errs []error
	for _, rel := range paths {
		target := filepath.Join(csgo, filepath.FromSlash(rel))
		if !safeSubPath(csgo, target) {
			continue
		}
		if _, err := os.Lstat(target); err != nil {
			continue // Owns may list paths a given release did not ship
		}
		if err := alignOwnership(target, uid, gid); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Sprintf("installed files could not be given to the game user (uid %d): %v", uid, errors.Join(errs...))
	}
	return ""
}

// alignAbsToGameOwner gives an absolute path (and everything below it) the game
// tree's owner. Editing a plugin config from the panel used to leave the file
// root-owned, so CounterStrikeSharp — running as the game user — could no
// longer rewrite it and silently lost the plugin's own updates.
//
// Failures are returned so the caller can decide; a config the plugin cannot
// write is worth telling the operator about.
func (in *Installer) alignAbsToGameOwner(paths ...string) error {
	csgo := in.cfg.CSGODir()
	uid, gid, ok := in.gameOwner()
	if !ok {
		return nil
	}
	var errs []error
	for _, p := range paths {
		if !safeSubPath(csgo, p) {
			continue
		}
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if err := alignOwnership(p, uid, gid); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
