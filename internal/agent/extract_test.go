package agent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
	tops, err := extractZip(readerAt(zipBytes), int64(len(zipBytes)), dest)
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

func TestExtractZipSlipRejected(t *testing.T) {
	zipBytes, err := makeZip(map[string][]byte{
		"../evil.txt": {1},
	})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := extractZip(readerAt(zipBytes), int64(len(zipBytes)), dest); err != ErrUnsafePath {
		t.Fatalf("want ErrUnsafePath, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "..", "evil.txt")); err == nil {
		t.Fatal("file escaped target dir")
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
	tops, err := extractTarGz(bytes.NewReader(raw.Bytes()), dest)
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
	if _, err := extractArchive("thing.rar", readerAt([]byte{0}), 1, t.TempDir()); err == nil {
		t.Fatal("expected error for unsupported type")
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
