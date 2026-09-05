package agent

import (
	"context"
	"errors"
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
	// sysd answers "which account does the game run as", which decides who
	// must own the files an install writes. Lazily created so tests can inject
	// a fake, and nil-safe because most installer work does not need it.
	sysd unitUserReader
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
	return &http.Client{Timeout: downloadTimeout, Transport: newTransport()}
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
			out[i].Installed = true
			out[i].InstalledVersion = s.Version
		}
	}
	// Second pass: an entry can only be blocked by something already known to
	// be installed, so dependents are resolved after the installed set is known.
	installed := func(id string) bool { _, ok := byName[id]; return ok }
	for i := range out {
		if !out[i].Installed {
			continue
		}
		for _, other := range in.catalog {
			if other.ID == out[i].ID || !installed(other.ID) {
				continue
			}
			for _, req := range other.Requires {
				if req == out[i].ID {
					out[i].RequiredBy = append(out[i].RequiredBy, other.Name)
					break
				}
			}
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
	// Warning carries a non-fatal problem (e.g. file ownership could not be
	// aligned). The install succeeded; the operator should still see it.
	Warning string `json:"warning,omitempty"`
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
	assetName, assetURLs, version, err := in.resolveArtifact(ctx, entry)
	if err != nil {
		return res, err
	}

	installedDeps := false
	for _, dep := range entry.Requires {
		if in.IsInstalled(dep) {
			continue
		}
		depName := dep
		if depEntry, ok := Find(in.catalog, dep); ok {
			depName = depEntry.Name
			step("installing dependency %s", depEntry.Name)
		}
		if _, err := in.install(ctx, dep, false, progress); err != nil {
			// Nesting "plugins: dependency X: plugins: dependency Y: plugins:
			// …" produced unreadable operator text. depError keeps the single
			// root cause and names the dependency that could not be installed.
			return res, depError(depName, err)
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
	if err := in.download(ctx, entry, assetURLs, tmpPath); err != nil {
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
	var warnings []string
	for _, pstep := range entry.PostInstall {
		step("applying %s", pstep)
		if err := in.runPostInstall(pstep); err != nil {
			if msg, ok := asWarning(err); ok {
				warnings = append(warnings, msg)
				step("warning: %s", msg)
				continue
			}
			return res, err
		}
	}

	// Record the csgo-relative paths to remove on uninstall. Owns is
	// authoritative when set: several archives write into shared trees like
	// addons/ that must never be deleted wholesale.
	owned := in.ownedPaths(entry, dest, tops)
	manifest := map[string]string{"artifact": assetName}
	for i, p := range owned {
		manifest[fmt.Sprintf("top%d", i)] = p
	}

	// The agent runs as root; the game does not. Hand the new files to whoever
	// owns the game tree so CounterStrikeSharp can write the configs and data
	// files it generates on first load.
	if warn := in.applyGameOwnership(owned); warn != "" {
		warnings = append(warnings, warn)
		step("warning: %s", warn)
	}
	res.Warning = strings.Join(warnings, "; ")

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

// resolveArtifact finds the download URLs for an entry: direct URL, pointer
// file (AlliedModders), or latest GitHub release asset matching the regex.
// More than one URL means mirrors of the same artifact, tried in order.
func (in *Installer) resolveArtifact(ctx context.Context, entry CatalogEntry) (name string, urls []string, version string, err error) {
	if entry.URL != "" {
		if entry.URLIsPointer {
			return in.resolvePointer(ctx, entry)
		}
		return filepath.Base(entry.URL), pointerCandidates(entry), "latest", nil
	}
	rel, err := in.gh.LatestRelease(ctx, entry.Repo)
	if err != nil {
		return "", nil, "", fmt.Errorf("plugins: %s: %w", entry.ID, err)
	}
	re, err := regexp.Compile(entry.AssetRegex)
	if err != nil {
		return "", nil, "", fmt.Errorf("plugins: %s bad asset regex: %w", entry.ID, err)
	}
	var reject *regexp.Regexp
	if entry.AssetReject != "" {
		reject, err = regexp.Compile(entry.AssetReject)
		if err != nil {
			return "", nil, "", fmt.Errorf("plugins: %s bad asset reject regex: %w", entry.ID, err)
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
		return matches[0].Name, []string{matches[0].URL}, rel.TagName, nil
	case 0:
		return "", nil, "", fmt.Errorf("plugins: %s: no asset matching %q in release %s (assets: %s)",
			entry.ID, entry.AssetRegex, rel.TagName, strings.Join(names, ", "))
	default:
		// Ambiguity means the catalog pattern no longer describes the release
		// (a new variant appeared). Guessing could install a Windows build or
		// a bundle that fights the dependency the agent installs itself.
		picked := make([]string, 0, len(matches))
		for _, a := range matches {
			picked = append(picked, a.Name)
		}
		return "", nil, "", fmt.Errorf("plugins: %s: %d assets match %q in release %s (%s) — the catalog entry needs a narrower pattern",
			entry.ID, len(matches), entry.AssetRegex, rel.TagName, strings.Join(picked, ", "))
	}
}

// resolvePointer reads an AlliedModders "-latest-" pointer file, whose body is
// the current build's filename, and resolves it against the pointer's dir.
//
// The pointer host sits behind Cloudflare and intermittently truncates the
// response ("unexpected EOF" for a 35-byte file). Because metamod is the root
// dependency of the whole catalog, one blip there used to fail every plugin
// install, so each mirror is retried and then the next mirror is tried.
func (in *Installer) resolvePointer(ctx context.Context, entry CatalogEntry) (name string, urls []string, version string, err error) {
	client := in.http
	if client == nil {
		client = newDownloadClient()
	}
	var errs []error
	for _, pointerURL := range pointerCandidates(entry) {
		var body []byte
		err := httpGet(ctx, client, pointerURL, nil, func(resp *http.Response) error {
			b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			if err != nil {
				return err
			}
			body = b
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", hostOf(pointerURL), err))
			continue
		}
		file := strings.TrimSpace(string(body))
		if file == "" || strings.ContainsAny(file, "/\\ \t\n") || strings.Contains(file, "..") {
			errs = append(errs, fmt.Errorf("%s: unexpected pointer body %q", hostOf(pointerURL), truncate(file, 60)))
			continue
		}
		// Every mirror serves the identical build, so the resolved filename is
		// appended to all of them: if the mirror that answered the pointer then
		// fails to serve the tarball, the next one still can.
		out := make([]string, 0, len(errs)+1)
		for _, cand := range pointerCandidates(entry) {
			out = append(out, dirURL(cand)+file)
		}
		// Try the mirror that just answered first.
		answered := dirURL(pointerURL) + file
		for i, u := range out {
			if u == answered && i != 0 {
				out[0], out[i] = out[i], out[0]
				break
			}
		}
		return file, out, pointerVersion(file), nil
	}
	return "", nil, "", fmt.Errorf("plugins: %s: could not read the latest-build pointer: %w",
		entry.ID, errors.Join(errs...))
}

// dirURL trims the last path segment, keeping the trailing slash.
func dirURL(raw string) string {
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		return raw[:i+1]
	}
	return raw
}

// pointerCandidates returns the pointer URL plus its known-good mirrors.
// metamodsource.net and sourcemm.net are official aliases of the same origin
// serving a byte-identical tree, so a Cloudflare edge that is unhealthy for one
// hostname is usually fine on another.
func pointerCandidates(entry CatalogEntry) []string {
	out := []string{entry.URL}
	for _, alt := range entry.URLMirrors {
		if alt != "" && alt != entry.URL {
			out = append(out, alt)
		}
	}
	return out
}

// pointerVersion turns "mmsource-2.0.0-git1411-linux.tar.gz" into
// "2.0.0-git1411" for display and state.
func pointerVersion(file string) string {
	version := file
	for _, cut := range []string{".tar.gz", ".tgz", ".zip"} {
		version = strings.TrimSuffix(version, cut)
	}
	version = strings.TrimPrefix(version, "mmsource-")
	version = strings.TrimSuffix(version, "-linux")
	if version == "" {
		return "latest"
	}
	return version
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// depError wraps a dependency failure once. Installing WeaponPaints pulls
// cssharp which pulls metamod, and the old recursive wrapping produced
// "plugins: dependency cssharp: plugins: dependency metamod: plugins: metamod:
// …" — three layers of prefix before the sentence that matters. Only the
// innermost dependency is named, and the root cause is preserved for errors.Is.
func depError(depName string, err error) error {
	var de *dependencyError
	if errors.As(err, &de) {
		return de // already attributed to the real culprit deeper down
	}
	return &dependencyError{dep: depName, err: err}
}

type dependencyError struct {
	dep string
	err error
}

func (e *dependencyError) Error() string {
	return fmt.Sprintf("%s is required first, and installing it failed: %v", e.dep, e.err)
}

func (e *dependencyError) Unwrap() error { return e.err }

// warning is a problem that must reach the operator without failing the
// install. A post-install step that could not finish (an upstream archive that
// stopped shipping a file, ownership that could not be aligned) still leaves a
// usable, uninstallable install behind — reporting it as an error would instead
// abandon the extracted files with no recorded state.
type warning struct{ msg string }

func (w *warning) Error() string { return w.msg }

func warnf(format string, args ...any) error {
	return &warning{msg: fmt.Sprintf(format, args...)}
}

// asWarning reports the operator-facing text when err is a warning.
func asWarning(err error) (string, bool) {
	var w *warning
	if errors.As(err, &w) {
		return w.msg, true
	}
	return "", false
}

func (in *Installer) download(ctx context.Context, entry CatalogEntry, urls []string, dest string) error {
	if len(urls) == 0 {
		return fmt.Errorf("plugins: no download url for %s", entry.ID)
	}
	client := in.http
	if client == nil {
		client = newDownloadClient()
	}
	var errs []error
	for _, url := range urls {
		headers := map[string]string{}
		// GitHub asset URLs honour the token for private/rate-limited repos.
		if in.gh != nil && in.gh.Token != "" && strings.Contains(url, "github") {
			headers["Authorization"] = "Bearer " + in.gh.Token
		}
		err := httpGet(ctx, client, url, headers, func(resp *http.Response) error {
			// Every attempt restarts the file: a retry after a truncated body
			// must not append to the bytes the failed attempt wrote.
			f, err := os.Create(dest)
			if err != nil {
				return err
			}
			_, err = copyCapped(f, resp, maxFileBytes)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			return err
		})
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", hostOf(url), err))
	}
	return fmt.Errorf("plugins: download %s: %w", entry.ID, errors.Join(errs...))
}

func (in *Installer) runPostInstall(step string) error {
	switch step {
	case "gameinfo-metamod":
		return patchGameinfoMetamod(filepath.Join(in.cfg.CSGODir(), "gameinfo.gi"))
	case "guidelines-off":
		return patchCoreGuidelines(filepath.Join(in.cfg.CSGODir(), "addons", "counterstrikesharp", "configs", "core.json"))
	case "wp-default-config":
		return in.writeWeaponPaintsDefaultConfig()
	case "wp-gamedata":
		return in.placeWeaponPaintsGamedata()
	case "whitelist-core-cfg":
		return writeWhitelistCoreCFG(filepath.Join(in.cfg.CFGDir(), "cs2whitelist", "core.cfg"))
	default:
		return fmt.Errorf("plugins: unknown post-install step %q", step)
	}
}

// Uninstall removes an installed component by deleting the paths recorded at
// install time. Paths are validated to be inside the csgo dir.
//
// It refuses to remove something other installed components need: uninstalling
// Metamod:Source from the panel used to succeed and silently stop every other
// plugin from loading, with no indication of why.
func (in *Installer) Uninstall(id string) error {
	state, err := in.store.GetPluginState(id)
	if err != nil {
		return err
	}
	if blockers := in.installedDependents(id); len(blockers) > 0 {
		return fmt.Errorf("plugins: %s is required by %s — uninstall %s first",
			displayName(in.catalog, id), strings.Join(blockers, ", "),
			pluralWord(len(blockers), "it", "them"))
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
	// Undo install-time edits to files cs2a does not own, so a reinstall starts
	// from a clean state and gameinfo.gi does not keep pointing at a search
	// path that no longer exists.
	if entry, ok := Find(in.catalog, id); ok {
		for _, step := range entry.PostInstall {
			if step == "gameinfo-metamod" {
				if err := unpatchGameinfoMetamod(filepath.Join(csgo, "gameinfo.gi")); err != nil {
					return err
				}
			}
		}
	}
	return in.store.DeletePluginState(id)
}

// installedDependents returns the display names of installed entries that
// require id.
func (in *Installer) installedDependents(id string) []string {
	var out []string
	for _, e := range in.catalog {
		if e.ID == id {
			continue
		}
		for _, req := range e.Requires {
			if req == id && in.IsInstalled(e.ID) {
				out = append(out, e.Name)
				break
			}
		}
	}
	return out
}

// displayName returns a catalog entry's human name, falling back to the id.
func displayName(catalog []CatalogEntry, id string) string {
	if e, ok := Find(catalog, id); ok {
		return e.Name
	}
	return id
}

// dependsOn reports whether id transitively requires dep. WeaponPaints requires
// cssharp which requires metamod, so uninstalling metamod during a WeaponPaints
// install must be refused too.
func dependsOn(catalog []CatalogEntry, id, dep string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(cur string) bool {
		if seen[cur] {
			return false // cycles in a hand-written catalog must not hang
		}
		seen[cur] = true
		e, ok := Find(catalog, cur)
		if !ok {
			return false
		}
		for _, req := range e.Requires {
			if req == dep || walk(req) {
				return true
			}
		}
		return false
	}
	return walk(id)
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
