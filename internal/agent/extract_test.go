package agent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func makeZip(files map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	// Close must run BEFORE reading the buffer: the zip central directory
	// is only written on Close.
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func readerAt(b []byte) io.ReaderAt {
	return bytes.NewReader(b)
}

func TestExtractZipBasic(t *testing.T) {
	zipBytes, err := makeZip(map[string][]byte{
		"addons/counterstrikesharp/api/Foo.dll":       {1, 2, 3},
		"addons/counterstrikesharp/bin/runner.so":     {4, 5},
		"addons/counterstrikesharp/configs/core.json": []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	tops, err := extractZip(readerAt(zipBytes), int64(len(zipBytes)), dest, 0)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(tops) != 1 || tops[0] != "addons" {
		t.Fatalf("tops = %v", tops)
	}
	got, err := os.ReadFile(filepath.Join(dest, "addons/counterstrikesharp/api/Foo.dll"))
	if err != nil || !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("file content wrong: %v %v", got, err)
	}
}

func TestExtractZipSlipContainment(t *testing.T) {
	zipBytes, err := makeZip(map[string][]byte{
		"../evil.txt":   {1},
		"/absolute.txt": {2},
	})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	tops, err := extractZip(readerAt(zipBytes), int64(len(zipBytes)), dest, 0)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// entries with traversal/absolute components must resolve INSIDE dest
	if _, err := os.Stat(filepath.Join(dest, "evil.txt")); err != nil {
		t.Fatalf("evil.txt not contained in dest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "absolute.txt")); err != nil {
		t.Fatalf("absolute.txt not contained in dest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "..", "evil.txt")); err == nil {
		t.Fatal("file escaped target dir")
	}
	if len(tops) != 2 || tops[0] != "absolute.txt" || tops[1] != "evil.txt" {
		t.Fatalf("tops = %v", tops)
	}
}

func TestExtractZipNulNameRejected(t *testing.T) {
	zipBytes, err := makeZip(map[string][]byte{"bad\x00name.txt": {1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extractZip(readerAt(zipBytes), int64(len(zipBytes)), t.TempDir(), 0); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("want ErrUnsafePath, got %v", err)
	}
}

func TestExtractTarGzBasic(t *testing.T) {
	var raw bytes.Buffer
	gw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gw)
	files := []struct {
		name string
		body string
		mode int64
	}{
		{"addons/metamod/metamod.2.cs2.so", "ELF", 0o755},
		{"addons/metamod/README.md", "readme", 0o644},
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	tops, err := extractTarGz(bytes.NewReader(raw.Bytes()), dest, 0)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(tops) != 1 || tops[0] != "addons" {
		t.Fatalf("tops = %v", tops)
	}
	so, err := os.Stat(filepath.Join(dest, "addons/metamod/metamod.2.cs2.so"))
	if err != nil {
		t.Fatal(err)
	}
	if so.Mode()&0o100 == 0 {
		t.Fatalf("exec bit lost: %v", so.Mode())
	}
}

func TestExtractArchiveUnsupported(t *testing.T) {
	if _, err := extractArchive("thing.rar", readerAt([]byte{0}), 1, t.TempDir(), 0); err == nil {
		t.Fatal("expected error for unsupported type")
	}
}

// Several upstream releases wrap everything in a version directory
// (SharpTimer-v0.4.0/addons/...). Strip must remove it so files land in the
// right place, and entries inside the stripped prefix alone are skipped.
func TestExtractZipStripComponents(t *testing.T) {
	zipBytes, err := makeZip(map[string][]byte{
		"SharpTimer-v0.4.0/addons/counterstrikesharp/plugins/X/X.dll": {9},
		"SharpTimer-v0.4.0/README.md":                                 []byte("hi"),
	})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	tops, err := extractZip(readerAt(zipBytes), int64(len(zipBytes)), dest, 1)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(tops) != 2 || tops[0] != "README.md" || tops[1] != "addons" {
		t.Fatalf("tops = %v", tops)
	}
	if _, err := os.Stat(filepath.Join(dest, "addons/counterstrikesharp/plugins/X/X.dll")); err != nil {
		t.Fatalf("stripped path wrong: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "SharpTimer-v0.4.0")); err == nil {
		t.Fatal("wrapper directory was not stripped")
	}
}

// archiveTops must report the layout without writing anything, and detectStrip
// must tell a release wrapper ("SharpTimer-v0.4.0/addons/…") apart from a bare
// plugin folder ("WeaponPaints/WeaponPaints.dll"), which IS the payload.
func TestArchiveTopsAndStripDetection(t *testing.T) {
	bare, err := makeZip(map[string][]byte{
		"WeaponPaints/WeaponPaints.dll": {1},
		"WeaponPaints/lang/en.json":     []byte("{}"),
	})
	if err != nil {
		t.Fatal(err)
	}
	tops, err := archiveTops("wp.zip", readerAt(bare), int64(len(bare)))
	if err != nil {
		t.Fatal(err)
	}
	if len(tops) != 1 || tops[0] != "WeaponPaints" {
		t.Fatalf("tops = %v", tops)
	}
	if got := detectStrip("wp.zip", readerAt(bare), int64(len(bare))); got != 0 {
		t.Fatalf("bare plugin folder must not be stripped, got %d", got)
	}

	wrapped, err := makeZip(map[string][]byte{
		"SharpTimer-v0.4.0/addons/counterstrikesharp/plugins/S/S.dll": {2},
		"SharpTimer-v0.4.0/README.md":                                 []byte("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := detectStrip("s.zip", readerAt(wrapped), int64(len(wrapped))); got != 1 {
		t.Fatalf("version wrapper must be stripped, got %d", got)
	}

	rooted, err := makeZip(map[string][]byte{
		"addons/metamod/cs2fixes.vdf": {3},
		"cfg/cs2fixes/cs2fixes.cfg":   {4},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := detectStrip("f.zip", readerAt(rooted), int64(len(rooted))); got != 0 {
		t.Fatalf("already-rooted archive must not be stripped, got %d", got)
	}
}

func TestPatchGameinfoMetamod(t *testing.T) {
	sample := `"GameInfo"
{
	game		"Counter-Strike 2"

	FileSystem
	{
		SteamAppId				730

		SearchPaths
		{
			Game				csgo_core
			Game				csgo
			Game				core
			Platform			platform
		}
	}
}
`
	dir := t.TempDir()
	gi := filepath.Join(dir, "gameinfo.gi")
	if err := os.WriteFile(gi, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patchGameinfoMetamod(gi); err != nil {
		t.Fatalf("patch: %v", err)
	}
	out, _ := os.ReadFile(gi)
	s := string(out)
	if !bytes.Contains(out, []byte("Game\tcsgo/addons/metamod")) {
		t.Fatalf("metamod line missing:\n%s", s)
	}
	// must appear on the line directly above the exact "Game csgo" line
	// (not the csgo_core line)
	reExact := regexp.MustCompile(`(?m)^\s*Game\t\t\t\tcsgo\s*$`)
	loc := reExact.FindStringIndex(s)
	idxM := strings.Index(s, "csgo/addons/metamod")
	if loc == nil || idxM < 0 || idxM > loc[0] {
		t.Fatalf("metamod line not immediately before Game csgo:\n%s", s)
	}
	// idempotent
	if err := patchGameinfoMetamod(gi); err != nil {
		t.Fatalf("second patch: %v", err)
	}
	out2, _ := os.ReadFile(gi)
	if got := bytes.Count(out2, []byte("csgo/addons/metamod")); got != 1 {
		t.Fatalf("patch not idempotent (%d occurrences)", got)
	}
}

func TestPatchGameinfoNoCSGO(t *testing.T) {
	dir := t.TempDir()
	gi := filepath.Join(dir, "gameinfo.gi")
	if err := os.WriteFile(gi, []byte("nothing here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patchGameinfoMetamod(gi); err == nil {
		t.Fatal("expected error when Game csgo line missing")
	}
}

func TestPatchCoreGuidelines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "configs", "core.json")
	if err := patchCoreGuidelines(p); err != nil {
		t.Fatalf("patch missing file: %v", err)
	}
	out, _ := os.ReadFile(p)
	if !bytes.Contains(out, []byte(`"FollowCS2ServerGuidelines": false`)) {
		t.Fatalf("flag not set:\n%s", out)
	}
	// existing file gets merged, other fields kept
	if err := os.WriteFile(p, []byte(`{"FollowCS2ServerGuidelines": true, "PublicChatTrigger": "!"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patchCoreGuidelines(p); err != nil {
		t.Fatal(err)
	}
	out, _ = os.ReadFile(p)
	if !bytes.Contains(out, []byte(`"FollowCS2ServerGuidelines": false`)) ||
		!bytes.Contains(out, []byte(`"PublicChatTrigger": "!"`)) {
		t.Fatalf("merge failed:\n%s", out)
	}
}
