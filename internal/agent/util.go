package agent

import (
	"os"
	"path/filepath"

	"cs2a/internal/fsatomic"
)

// atomicWrite writes data to path via a temp file + rename so readers never
// observe partial files, keeping the destination's owner.
//
// The rename replaces the file rather than editing it, so the result is a brand
// new inode owned by whoever ran the agent — root. Every write into the game
// tree therefore used to strip the game user's ownership: patching cssharp's
// core.json made it root-owned and cssharp, running unprivileged, could no
// longer rewrite it.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	return fsatomic.Write(path, data, perm)
}

// statOwner reports the owner of path without following symlinks.
func statOwner(path string) (uid, gid int, ok bool) {
	return fsatomic.Owner(path)
}

// ownerToInherit is the ownership atomicWrite gives a file it replaces or
// creates. Exposed here for the tests that pin the behaviour.
func ownerToInherit(path string) (uid, gid int, ok bool) {
	if os.Geteuid() != 0 {
		return 0, 0, false
	}
	if u, g, found := statOwner(path); found {
		return u, g, u != 0 || g != 0
	}
	u, g, found := statOwner(dirOf(path))
	if !found {
		return 0, 0, false
	}
	return u, g, u != 0 || g != 0
}

// ensureDir creates dir and all parents.
func ensureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// safeSubPath verifies target is inside root (used before destructive ops).
func safeSubPath(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && rel != "." && !filepath.IsAbs(rel) && !containsDotDot(rel)
}

func containsDotDot(rel string) bool {
	for _, part := range splitPath(rel) {
		if part == ".." {
			return true
		}
	}
	return false
}

func splitPath(p string) []string {
	var out []string
	for p != "." && p != "/" && p != "" {
		out = append(out, filepath.Base(p))
		p = filepath.Dir(p)
	}
	// reverse
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
