package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// maxArchiveBytes caps any single extracted file and the whole archive.
const (
	maxFileBytes    = 4 << 30 // 4 GiB (cssharp with-runtime is ~200MB)
	maxTotalExtract = 8 << 30
)

// ErrUnsafePath is returned when an archive entry would escape the target.
var ErrUnsafePath = errors.New("archive: unsafe entry path")

// sanitizeJoin safely joins an archive entry name onto a destination dir,
// rejecting absolute paths, traversal, and windows-style names.
func sanitizeJoin(dest, name string) (string, error) {
	name = filepath.ToSlash(name)
	if strings.Contains(name, "\x00") {
		return "", ErrUnsafePath
	}
	clean := path.Clean("/" + name) // "/../x" -> "/x"
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return "", ErrUnsafePath
	}
	if strings.HasPrefix(clean, "..") {
		return "", ErrUnsafePath
	}
	return filepath.Join(dest, filepath.FromSlash(clean)), nil
}

// extractZip unpacks a .zip into dest and returns the set of top-level
// first path components written (relative to dest).
func extractZip(src io.ReaderAt, size int64, dest string) ([]string, error) {
	zr, err := zip.NewReader(src, size)
	if err != nil {
		return nil, fmt.Errorf("archive: open zip: %w", err)
	}
	var total int64
	tops := map[string]struct{}{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			tops[topOf(f.Name)] = struct{}{}
			continue
		}
		target, err := sanitizeJoin(dest, f.Name)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrUnsafePath, f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := extractFile(f, target, &total); err != nil {
			return nil, err
		}
		tops[topOf(f.Name)] = struct{}{}
	}
	return sortedTops(tops), nil
}

// topOf returns the first path component of an archive entry name.
func topOf(name string) string {
	name = filepath.ToSlash(name)
	clean := path.Clean("/" + name)
	clean = strings.TrimPrefix(clean, "/")
	i := strings.Index(clean, "/")
	if i < 0 {
		return clean
	}
	return clean[:i]
}

func sortedTops(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" && k != "." {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func extractFile(f *zip.File, target string, total *int64) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("archive: open %s: %w", f.Name, err)
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(rc, maxFileBytes+1))
	closeErr := out.Close()
	if err != nil {
		return fmt.Errorf("archive: write %s: %w", f.Name, err)
	}
	if closeErr != nil {
		return closeErr
	}
	if n > maxFileBytes {
		return fmt.Errorf("archive: %s exceeds size cap", f.Name)
	}
	*total += n
	if *total > maxTotalExtract {
		return fmt.Errorf("archive: total extract exceeds cap")
	}
	return nil
}

// extractTarGz unpacks a .tar.gz into dest, returning top-level components.
func extractTarGz(src io.Reader, dest string) ([]string, error) {
	gz, err := gzip.NewReader(io.LimitReader(src, maxTotalExtract))
	if err != nil {
		return nil, fmt.Errorf("archive: gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	tops := map[string]struct{}{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return sortedTops(tops), nil
		}
		if err != nil {
			return nil, fmt.Errorf("archive: tar: %w", err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeSymlink:
			// symlinks are stored as regular files (content copy) to keep
			// extraction side-effect free
		default:
			continue
		}
		target, err := sanitizeJoin(dest, hdr.Name)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrUnsafePath, hdr.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		tops[topOf(hdr.Name)] = struct{}{}
		mode := os.FileMode(0o755)
		if hdr.Typeflag == tar.TypeReg && hdr.FileInfo().Mode().IsRegular() {
			// preserve the exec bit only; avoid weird permissions from the wire
			if hdr.FileInfo().Mode()&0o100 != 0 {
				mode = 0o755
			} else {
				mode = 0o644
			}
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			return nil, err
		}
		n, err := io.Copy(out, io.LimitReader(tr, maxFileBytes+1))
		closeErr := out.Close()
		if err != nil {
			return nil, fmt.Errorf("archive: write %s: %w", hdr.Name, err)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if n > maxFileBytes {
			return nil, fmt.Errorf("archive: %s exceeds size cap", hdr.Name)
		}
		total += n
		if total > maxTotalExtract {
			return nil, fmt.Errorf("archive: total extract exceeds cap")
		}
	}
}

// extractArchive detects zip vs tar.gz by name and unpacks into dest,
// returning the top-level components written.
func extractArchive(name string, src io.ReaderAt, size int64, dest string) ([]string, error) {
	lname := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lname, ".zip"):
		return extractZip(src, size, dest)
	case strings.HasSuffix(lname, ".tar.gz"), strings.HasSuffix(lname, ".tgz"):
		// tar needs a sequential reader; wrap the ReaderAt
		return extractTarGz(io.NewSectionReader(src, 0, size), dest)
	default:
		return nil, fmt.Errorf("archive: unsupported artifact type %q", name)
	}
}
