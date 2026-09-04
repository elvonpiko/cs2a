package agent

import (
	"os"
	"path/filepath"
)

// atomicWrite writes data to path via a temp file + rename so readers never
// observe partial files.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := dirOf(path)
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
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, perm) // rename may have kept temp file mode
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
