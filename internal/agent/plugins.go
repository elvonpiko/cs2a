package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
	httpc := &http.Client{Timeout: 10 * time.Minute}
	if gh != nil {
		httpc = gh.HTTP // reuse transport/timeouts (also test-rewritable)
	}
	return &Installer{
		cfg:     cfg,
		store:   store,
		catalog: catalog,
		gh:      gh,
		http:    httpc,
	}
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
	assetName, assetURL, version, err := in.resolveArtifact(ctx, entry)
	if err != nil {
		return res, err
	}

	installedDeps := false
	for _, dep := range entry.Requires {
		if in.IsInstalled(dep) {
			continue
		}
		if _, err := in.Install(ctx, dep, false); err != nil {
			return res, fmt.Errorf("plugins: dependency %s: %w", dep, err)
		}
		installedDeps = true
	}
	res.InstalledDeps = installedDeps

	// download
	if err := ensureDir(in.cfg.PluginCache); err != nil {
		return res, err
	}
	tmpPath := filepath.Join(in.cfg.PluginCache, "cs2a-dl-"+id+"-"+version)
	if err := in.download(ctx, entry, assetURL, tmpPath); err != nil {
		return res, err
	}
	defer os.Remove(tmpPath)

	// extract into csgo dir
	f, err := os.Open(tmpPath)
	if err != nil {
		return res, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return res, err
	}
	tops, err := extractArchive(assetName, f, fi.Size(), in.cfg.CSGODir())
	if err != nil {
		return res, fmt.Errorf("plugins: extract %s: %w", assetName, err)
	}

	// post-install steps
	for _, step := range entry.PostInstall {
		if err := in.runPostInstall(step); err != nil {
			return res, err
		}
	}

	// record state with manifest of top-level paths created
	manifest := map[string]string{"artifact": assetName}
	for i, t := range tops {
		manifest[fmt.Sprintf("top%d", i)] = t
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

// resolveArtifact finds the download URL for an entry: direct URL (metamod)
// or latest GitHub release asset matching the regex.
func (in *Installer) resolveArtifact(ctx context.Context, entry CatalogEntry) (name, url, version string, err error) {
	if entry.URL != "" {
		name = filepath.Base(entry.URL)
		return name, entry.URL, "latest", nil
	}
	rel, err := in.gh.LatestRelease(ctx, entry.Repo)
	if err != nil {
		return "", "", "", fmt.Errorf("plugins: %s: %w", entry.ID, err)
	}
	re, err := regexp.Compile(entry.AssetRegex)
	if err != nil {
		return "", "", "", fmt.Errorf("plugins: %s bad asset regex: %w", entry.ID, err)
	}
	for _, a := range rel.Assets {
		if re.MatchString(a.Name) {
			return a.Name, a.URL, rel.TagName, nil
		}
	}
	return "", "", "", fmt.Errorf("plugins: %s: no asset matching %q in release %s (assets: %d)", entry.ID, entry.AssetRegex, rel.TagName, len(rel.Assets))
}

func (in *Installer) download(ctx context.Context, entry CatalogEntry, url, dest string) error {
	var body io.Reader
	if url == "" {
		return fmt.Errorf("plugins: empty url for %s", entry.ID)
	}
	// reuse GHClient for github-hosted assets (honors token), plain http otherwise
	if in.gh != nil && containsStr(url, "github") {
		f, err := os.Create(dest)
		if err != nil {
			return err
		}
		if err := in.gh.Download(ctx, url, f, maxFileBytes); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "cs2a-agent")
	resp, err := in.http.Do(req)
	if err != nil {
		return fmt.Errorf("plugins: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("plugins: download status %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxFileBytes+1)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (in *Installer) runPostInstall(step string) error {
	switch step {
	case "gameinfo-metamod":
		return patchGameinfoMetamod(filepath.Join(in.cfg.CSGODir(), "gameinfo.gi"))
	case "guidelines-off":
		return patchCoreGuidelines(filepath.Join(in.cfg.CSGODir(), "addons", "counterstrikesharp", "configs", "core.json"))
	default:
		return fmt.Errorf("plugins: unknown post-install step %q", step)
	}
}

// Uninstall removes an installed component by deleting the top-level paths
// recorded at install time. Paths are validated to be inside the csgo dir.
func (in *Installer) Uninstall(id string) error {
	state, err := in.store.GetPluginState(id)
	if err != nil {
		return err
	}
	csgo := in.cfg.CSGODir()
	for k, v := range state.Manifest {
		if k != "artifact" {
			target := filepath.Join(csgo, v)
			if !safeSubPath(csgo, target) {
				return fmt.Errorf("plugins: refusing to remove %q (outside csgo dir)", target)
			}
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("plugins: remove %s: %w", target, err)
			}
		}
	}
	return in.store.DeletePluginState(id)
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && indexOfStr(s, sub) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
