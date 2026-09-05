// Package fsatomic writes files atomically without silently changing who owns
// them or who may read them.
//
// The agent runs as root; the game server does not. A plain "write a temp file
// and rename it" replaces the destination with a brand new inode owned by root
// and, because os.CreateTemp creates with mode 0600, readable by nobody else.
// Doing that to the game's own server.cfg is not a cosmetic problem: the engine
// can no longer read it, so rcon_password stops being applied and RCON dies on
// the next map load — the panel's "Repair RCON" button was itself able to cause
// exactly the failure it exists to fix.
package fsatomic

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Write writes data to path atomically with the given mode, preserving the
// destination's owner. Use it when the mode is part of the contract (a secrets
// file that must stay 0600).
func Write(path string, data []byte, perm fs.FileMode) error {
	return write(path, data, perm, false)
}

// WriteKeepMode writes data to path atomically, keeping the mode and owner the
// file already has. fallback applies only when the file does not exist yet.
// Use it for files that belong to the operator or to the game.
func WriteKeepMode(path string, data []byte, fallback fs.FileMode) error {
	return write(path, data, fallback, true)
}

func write(path string, data []byte, perm fs.FileMode, keepMode bool) error {
	if keepMode {
		if fi, err := os.Stat(path); err == nil {
			perm = fi.Mode().Perm()
		}
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cs2a-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Mode and owner are set on the temp file, before the rename, so the
	// destination is never visible with the wrong ones.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if uid, gid, ok := ownerToInherit(path); ok {
		// Best effort: a file with the wrong owner still beats no file, and
		// callers that care report ownership problems separately.
		_ = tmp.Chown(uid, gid)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, perm) // rename may have kept the temp file's mode
}

// ownerToInherit reports the uid/gid the written file should belong to: the
// owner the file already has, or — when creating it — the owner of the directory
// holding it. A root-owned answer is reported as not ok, since the writer is
// already root and the chown would be a no-op.
func ownerToInherit(path string) (uid, gid int, ok bool) {
	if os.Geteuid() != 0 {
		return 0, 0, false // only root may give a file away
	}
	if u, g, found := Owner(path); found {
		return u, g, u != 0 || g != 0
	}
	u, g, found := Owner(filepath.Dir(path))
	if !found {
		return 0, 0, false
	}
	return u, g, u != 0 || g != 0
}

// Owner reports the uid/gid owning path, without following symlinks. ok is
// false for a missing path or a platform whose stat carries no Unix ids.
func Owner(path string) (uid, gid int, ok bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, 0, false
	}
	st, isUnix := fi.Sys().(*syscall.Stat_t)
	if !isUnix {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
