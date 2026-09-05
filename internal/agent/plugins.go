package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Installer resolves catalog entries against live releases, downloads and
// extracts them into the CS2 csgo directory, and records state.
type Installer struct {
	cfg     Config
	store   *Store
	catalog []CatalogEntry
	gh      *GHClient
	http    *http.Client
}

// NewInstaller wires an installer for the given catalog.
func NewInstaller(cfg Config, store *Store, catalog []CatalogEntry, gh *GHClient) *Installer {
	httpc := newDownloadClient()
	if gh != nil {
		// Share the transport so tests can rewrite it, but keep the long
		// download budget: gh.HTTP's own timeout is for API calls.
		httpc = &http.Client{Transport: gh.HTTP.Transport, Timeout: downloadTimeout}
	}
	return &Installer{
		cfg:     cfg,
		store:   store,
		catalog: catalog,
		gh:      gh,
		http:    httpc,
	}
}

// downloadTimeout bounds a single artifact download. CounterStrikeSharp's
// with-runtime zip is ~50 MB, so the 20 s API timeout is far too small.
const downloadTimeout = 15 * time.Minute

func newDownloadClient() *http.Client {
	return &http.Client{Timeout: downloadTimeout}
}

// Catalog returns the installable catalog annotated with installed state.
func (in *Installer) Catalog() ([]CatalogEntry, error) {
	states, err := in.store.ListPluginStates()
	if err != nil {
		return nil, err
	}
	byName := map[string]PluginState{}
	for _, s := range states {
		byName[s.Name] = s
	}
	out := make([]CatalogEntry, len(in.catalog))
	for i, e := range in.catalog {
		out[i] = e
		if s, ok := byName[e.ID]; ok {
			out[i].Description = fmt.Sprintf("[installed %s] %s", s.Version, e.Description)
		}
	}
	return out, nil
}

// IsInstalled reports whether an entry is recorded as installed.
func (in *Installer) IsInstalled(id string) bool {
	_, err := in.store.GetPluginState(id)
	return err == nil
}

// InstallResult reports what happened.
type InstallResult struct {
	ID              string `json:"id"`
	Version         string `json:"version"`
	RequiresRestart bool   `json:"requires_restart"`
	InstalledDeps   bool   `json:"installed_deps"`
}

// Install installs an entry and its dependencies (depth-first), extracts the
// artifact into the csgo directory and records state. Installing an already
// installed component is a no-op unless force is set.
func (in *Installer) Install(ctx context.Context, id string, force bool) (InstallResult, error) {
	return in.install(ctx, id, force, nil)
}

// InstallProgress is Install with a progress callback for the job runner.
func (in *Installer) InstallProgress(ctx context.Context, id string, force bool, progress func(string)) (InstallResult, error) {
	return in.install(ctx, id, force, progress)
}

func (in *Installer) install(ctx context.Context, id string, force bool, progress func(string)) (InstallResult, error) {
	step := func(format string, args ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, args...))
		}
	}
	var res InstallResult
	entry, ok := Find(in.catalog, id)
	if !ok {
		return res, fmt.Errorf("plugins: unknown catalog entry %q", id)
	}
	res.ID = id

	if existing, err := in.store.GetPluginState(id); err == nil && !force {
		res.Version = existing.Version
		res.RequiresRestart = entry.Kind != KindCSSharpPlugin
		return res, nil
	}

	// resolve the artifact (before touching deps so failures are cheap)
	step("resolving latest release of %s", entry.Name)
	assetName, assetURL, version, err := in.resolveArtifact(ctx, entry)
	if err != nil {
		return res, err
	}

	installedDeps := false
	for _, dep := range entry.Requires {
		if in.IsInstalled(dep) {
			continue
		}
		if depEntry, ok := Find(in.catalog, dep); ok {
			step("installing dependency %s", depEntry.Name)
		}
		if _, err := in.install(ctx, dep, false, progress); err != nil {
			return res, fmt.Errorf("plugins: dependency %s: %w", dep, err)
		}
		installedDeps = true
	}
	res.InstalledDeps = installedDeps

	// download
	if err := ensureDir(in.cfg.PluginCache); err != nil {
		return res, err
	}
	step("downloading %s %s", entry.Name, version)
	tmpPath := filepath.Join(in.cfg.PluginCache, "cs2a-dl-"+id+"-"+sanitizeFileName(version))
	if err := in.download(ctx, entry, assetURL, tmpPath); err != nil {
		return res, err
	}
	defer os.Remove(tmpPath)

	// extract into the destination the entry declares
	f, err := os.Open(tmpPath)
	if err != nil {
		return res, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return res, err
	}
	dest, err := in.destDir(entry)
	if err != nil {
		return res, err
	}
	if err := ensureDir(dest); err != nil {
		return res, err
	}
	strip := entry.Strip
	if strip == 0 {
		// Auto-detect a release wrapper directory (e.g. SharpTimer-v0.4.0/…)
		// so a release that starts adding one still installs correctly.
		strip = detectStrip(assetName, f, fi.Size())
	}
	step("installing %s into %s", entry.Name, relTo(in.cfg.CSGODir(), dest))
	tops, err := extractArchive(assetName, f, fi.Size(), dest, strip)
	if err != nil {
		return res, fmt.Errorf("plugins: extract %s: %w", assetName, err)
	}

	// post-install steps
	for _, pstep := range entry.PostInstall {
		step("applying %s", pstep)
		if err := in.runPostInstall(pstep); err != nil {
			return res, err
		}
	}

	// Record the csgo-relative paths to remove on uninstall. Owns is
	// authoritative when set: several archives write into shared trees like
	// addons/ that must never be deleted wholesale.
	manifest := map[string]string{"artifact": assetName}
	for i, p := range in.ownedPaths(entry, dest, tops) {
		manifest[fmt.Sprintf("top%d", i)] = p
	}
	res.Version = version
	res.RequiresRestart = entry.Kind != KindCSSharpPlugin
	if err := in.store.SetPluginState(PluginState{
		Name:     id,
		Version:  version,
		Status:   "installed",
		Manifest: manifest,
	}); err != nil {
		return res, err
	}
	return res, nil
}

// destDir resolves an entry's extraction directory, guarding against escapes.
func (in *Installer) destDir(entry CatalogEntry) (string, error) {
	csgo := in.cfg.CSGODir()
	if entry.Dest == "" {
		return csgo, nil
	}
	dest := filepath.Join(csgo, filepath.FromSlash(entry.Dest))
	if !safeSubPath(csgo, dest) && dest != csgo {
		return "", fmt.Errorf("plugins: %s has an unsafe dest %q", entry.ID, entry.Dest)
	}
	return dest, nil
}

// ownedPaths returns csgo-relative paths for uninstall.
func (in *Installer) ownedPaths(entry CatalogEntry, dest string, tops []string) []string {
	if len(entry.Owns) > 0 {
		out := make([]string, 0, len(entry.Owns))
		for _, p := range entry.Owns {
			out = append(out, path.Clean(p))
		}
		return out
	}
	prefix := relTo(in.cfg.CSGODir(), dest)
	out := make([]string, 0, len(tops))
	for _, t := range tops {
		if prefix == "" || prefix == "." {
			out = append(out, t)
			continue
		}
		out = append(out, path.Join(prefix, t))
	}
	return out
}

// relTo returns target relative to base in slash form ("" when equal).
func relTo(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// sanitizeFileName makes a release tag safe for use in a cache filename.
var reUnsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeFileName(s string) string {
	s = reUnsafeName.ReplaceAllString(s, "-")
	if len(s) > 64 {
		s = s[:64]
	}
	if s == "" {
		return "latest"
	}
	return s
}

// resolveArtifact finds the download URL for an entry: direct URL, pointer
// file (AlliedModders), or latest GitHub release asset matching the regex.
func (in *Installer) resolveArtifact(ctx context.Context, entry CatalogEntry) (name, url, version string, err error) {
	if entry.URL != "" {
		if entry.URLIsPointer {
			return in.resolvePointer(ctx, entry)
		}
		return filepath.Base(entry.URL), entry.URL, "latest", nil
	}
	rel, err := in.gh.LatestRelease(ctx, entry.Repo)
	if err != nil {
		return "", "", "", fmt.Errorf("plugins: %s: %w", entry.ID, err)
	}
	re, err := regexp.Compile(entry.AssetRegex)
	if err != nil {
		return "", "", "", fmt.Errorf("plugins: %s bad asset regex: %w", entry.ID, err)
	}
	var reject *regexp.Regexp
	if entry.AssetReject != "" {
		reject, err = regexp.Compile(entry.AssetReject)
		if err != nil {
			return "", "", "", fmt.Errorf("plugins: %s bad asset reject regex: %w", entry.ID, err)
		}
	}
	var matches []GHAsset
	for _, a := range rel.Assets {
		if !re.MatchString(a.Name) {
			continue
		}
		if reject != nil && reject.MatchString(a.Name) {
			continue
		}
		matches = append(matches, a)
	}
	names := make([]string, 0, len(rel.Assets))
	for _, a := range rel.Assets {
		names = append(names, a.Name)
	}
	switch len(matches) {
	case 1:
		return matches[0].Name, matches[0].URL, rel.TagName, nil
	case 0:
		return "", "", "", fmt.Errorf("plugins: %s: no asset matching %q in release %s (assets: %s)",
			entry.ID, entry.AssetRegex, rel.TagName, strings.Join(names, ", "))
	default:
		// Ambiguity means the catalog pattern no longer describes the release
		// (a new variant appeared). Guessing could install a Windows build or
		// a bundle that fights the dependency the agent installs itself.
		picked := make([]string, 0, len(matches))
		for _, a := range matches {
			picked = append(picked, a.Name)
		}
		return "", "", "", fmt.Errorf("plugins: %s: %d assets match %q in release %s (%s) — the catalog entry needs a narrower pattern",
			entry.ID, len(matches), entry.AssetRegex, rel.TagName, strings.Join(picked, ", "))
	}
}

// resolvePointer reads an AlliedModders "-latest-" pointer file, whose body is
// the current build's filename, and resolves it against the pointer's dir.
func (in *Installer) resolvePointer(ctx context.Context, entry CatalogEntry) (name, url, version string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.URL, nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("User-Agent", "cs2a-agent")
	client := in.http
	if client == nil {
		client = newDownloadClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("plugins: %s: read pointer: %w", entry.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("plugins: %s: pointer status %d", entry.ID, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", "", "", fmt.Errorf("plugins: %s: read pointer: %w", entry.ID, err)
	}
	file := strings.TrimSpace(string(body))
	if file == "" || strings.ContainsAny(file, "/\\ \t\n") || strings.Contains(file, "..") {
		return "", "", "", fmt.Errorf("plugins: %s: unexpected pointer body %q", entry.ID, truncate(file, 60))
	}
	base := entry.URL
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[:i+1]
	}
	// The pointer body carries the version: mmsource-2.0.0-git1411-linux.tar.gz
	version = file
	for _, cut := range []string{".tar.gz", ".tgz", ".zip"} {
		version = strings.TrimSuffix(version, cut)
	}
	version = strings.TrimPrefix(version, "mmsource-")
	version = strings.TrimSuffix(version, "-linux")
	if version == "" {
		version = "latest"
	}
	return file, base + file, version, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (in *Installer) download(ctx context.Context, entry CatalogEntry, url, dest string) error {
	if url == "" {
		return fmt.Errorf("plugins: empty url for %s", entry.ID)
	}
	// reuse GHClient for github-hosted assets (honors token), plain http otherwise
	if in.gh != nil && strings.Contains(url, "github") {
		f, err := os.Create(dest)
		if err != nil {
			return err
		}
		if err := in.gh.DownloadWith(ctx, in.http, url, f, maxFileBytes); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "cs2a-agent")
	client := in.http
	if client == nil {
		client = newDownloadClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("plugins: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("plugins: download status %d for %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		f.Close()
		return err
	}
	if n > maxFileBytes {
		f.Close()
		return fmt.Errorf("plugins: artifact exceeds %d byte cap", int64(maxFileBytes))
	}
	return f.Close()
}

func (in *Installer) runPostInstall(step string) error {
	switch step {
	case "gameinfo-metamod":
		return patchGameinfoMetamod(filepath.Join(in.cfg.CSGODir(), "gameinfo.gi"))
	case "guidelines-off":
		return patchCoreGuidelines(filepath.Join(in.cfg.CSGODir(), "addons", "counterstrikesharp", "configs", "core.json"))
	case "wp-default-config":
		return in.writeWeaponPaintsDefaultConfig()
	case "whitelist-core-cfg":
		return writeWhitelistCoreCFG(filepath.Join(in.cfg.CFGDir(), "cs2whitelist", "core.cfg"))
	default:
		return fmt.Errorf("plugins: unknown post-install step %q", step)
	}
}

// Uninstall removes an installed component by deleting the paths recorded at
// install time. Paths are validated to be inside the csgo dir.
func (in *Installer) Uninstall(id string) error {
	state, err := in.store.GetPluginState(id)
	if err != nil {
		return err
	}
	csgo := in.cfg.CSGODir()
	for k, v := range state.Manifest {
		if k == "artifact" {
			continue
		}
		target := filepath.Join(csgo, filepath.FromSlash(v))
		if !safeSubPath(csgo, target) {
			return fmt.Errorf("plugins: refusing to remove %q (outside csgo dir)", target)
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("plugins: remove %s: %w", target, err)
		}
	}
	return in.store.DeletePluginState(id)
}
