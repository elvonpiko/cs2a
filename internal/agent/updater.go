package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CS2AppID is Steam's app id for the Counter-Strike 2 dedicated server.
const CS2AppID = 730

// Update check cadence. Steam ships client updates whenever it likes; a
// server left on a 2019-era cron schedule that checked once a night used to
// be fine, but "client out of date" complaints are about the window between
// a client patch and the server catching up. A few hours bounds that window
// without hammering Steam (which rate-limits anonymous app_info requests).
const (
	updateCheckInterval = 6 * time.Hour
	// firstUpdateCheckDelay gives the machine time to finish booting before
	// the agent starts spawning steamcmd — on a fresh boot systemd is still
	// starting units and a steamcmd that loses the race reports nonsense.
	firstUpdateCheckDelay = 2 * time.Minute
	// remoteBuildCacheTTL bounds how stale a "no update" answer may be when
	// the operator clicks "check now" is not involved — the periodic loop
	// refreshes it anyway.
	remoteBuildCacheTTL = 30 * time.Minute
)

// UpdateCheckError reports that a comparison could not be made (steamcmd
// missing, Steam unreachable, manifest unreadable) — distinct from "no
// update available", which is a valid answer.
type UpdateCheckError struct{ Reason string }

func (e *UpdateCheckError) Error() string { return e.Reason }

// UpdateInfo is the agent's current knowledge of the server's build versus
// Steam's. Zero-value means "never checked".
type UpdateInfo struct {
	InstalledBuild string `json:"installed_build,omitempty"`
	LatestBuild    string `json:"latest_build,omitempty"`
	// Available is true when both builds are known and differ.
	Available bool `json:"available"`
	// AvailableSince is when the difference was first seen, so the panel can
	// say "for 3 days" instead of an unexplained badge.
	AvailableSince time.Time `json:"available_since,omitempty"`
	// PendingPlayers is true when an update was found but the server had
	// players on it, so auto-update is waiting for it to empty.
	PendingPlayers bool `json:"pending_players,omitempty"`
	// Updating is true while an update job runs.
	Updating bool `json:"updating"`
	// AutoUpdate mirrors the config flag so the panel can say whether the
	// badge means "will install itself" or "needs your click".
	AutoUpdate bool `json:"auto_update"`
	// LastCheck is the last successful comparison; zero when never.
	LastCheck time.Time `json:"last_check,omitempty"`
	// LastError is the last check/update failure, for the panel to surface.
	LastError string `json:"last_error,omitempty"`
}

// Updater keeps the CS2 install current against Steam's build for app 730.
//
// Detection follows the LinuxGSM method: the local appmanifest_730.acf holds
// the installed buildid; steamcmd's app_info_print holds Steam's current one
// for the public branch. Applying an update is steamcmd's own app_update —
// the exact command the installer ran — with the game server stopped first,
// because steamcmd replaces files the server has open.
type Updater struct {
	cfg  Config
	sysd ServiceController

	mu             sync.Mutex
	remoteBuild    string
	remoteFetched  time.Time
	remoteFetchErr string
	availableSince time.Time
	pendingPlayers bool
	lastCheck      time.Time
	lastErr        string

	// steamcmd resolution result, cached once.
	steamcmdOnce sync.Once
	steamcmdPath string
	steamcmdErr  error
	// run is the steamcmd execution seam: production shells out to the
	// resolved binary as the game user; tests swap it for a fake that
	// rewrites the manifest. nil until first use, then pinned.
	run func(ctx context.Context, timeout time.Duration, args ...string) (string, error)

	// humanPlayers reports how many humans are connected, for the
	// auto-update "don't kick anyone" rule. nil means "unknown — treat as
	// zero and let the stop/restart decide".
	humanPlayers func() int
	// wasActive is captured when an update starts: the service is restored
	// to the state it was found in, so updating an offline server leaves it
	// offline.
	wasActive bool
	// updating is true while an update job runs (set by the API layer).
	updating bool
}

// NewUpdater builds the update service. humanPlayers may be nil.
func NewUpdater(cfg Config, sysd ServiceController, humanPlayers func() int) *Updater {
	return &Updater{cfg: cfg, sysd: sysd, humanPlayers: humanPlayers, wasActive: false}
}

// appManifestPath is where steamcmd records the installed build of app 730.
func (u *Updater) appManifestPath() string {
	return filepath.Join(u.cfg.CS2Dir, fmt.Sprintf("appmanifest_%d.acf", CS2AppID))
}

// manifestBuildRe matches `"buildid"		"1234567"`.
var manifestBuildRe = regexp.MustCompile(`"buildid"\s+"(\d+)"`)

// LocalBuildID reads the installed build out of the app manifest.
func (u *Updater) LocalBuildID() (string, error) {
	data, err := os.ReadFile(u.appManifestPath())
	if err != nil {
		return "", &UpdateCheckError{Reason: "no appmanifest_730.acf in " + u.cfg.CS2Dir + " — the install was not done by steamcmd"}
	}
	m := manifestBuildRe.FindSubmatch(data)
	if m == nil {
		return "", &UpdateCheckError{Reason: "appmanifest_730.acf has no buildid"}
	}
	return string(m[1]), nil
}

// steamcmdCandidates are the places bootstrap.sh installs or finds steamcmd.
func steamcmdCandidates(homeUser string) []string {
	list := []string{"steamcmd", "/usr/games/steamcmd", "/usr/bin/steamcmd", "/opt/steamcmd/steamcmd.sh"}
	if homeUser != "" {
		list = append(list,
			filepath.Join("/home", homeUser, "steamcmd", "steamcmd.sh"),
			filepath.Join("/home", homeUser, ".local/share/Steam/steamcmd/steamcmd.sh"),
		)
	}
	return list
}

// SteamCMDPath locates the steamcmd binary, once per process. Empty path with
// a non-nil error means "cannot update".
func (u *Updater) SteamCMDPath() (string, error) {
	u.steamcmdOnce.Do(func() {
		// An explicit config path wins, as on servers where the operator
		// installed steamcmd somewhere exotic.
		if p := u.cfg.SteamCMDPath; p != "" {
			if _, err := os.Stat(p); err != nil {
				u.steamcmdErr = &UpdateCheckError{Reason: "steamcmd_path " + p + " does not exist"}
				return
			}
			u.steamcmdPath = p
			return
		}
		user := u.gameUser()
		for _, c := range steamcmdCandidates(user) {
			if c == "steamcmd" {
				if p, err := exec.LookPath("steamcmd"); err == nil {
					u.steamcmdPath = p
					return
				}
				continue
			}
			if _, err := os.Stat(c); err == nil {
				u.steamcmdPath = c
				return
			}
		}
		u.steamcmdErr = &UpdateCheckError{Reason: "steamcmd not found — install it or set steamcmd_path in the agent config"}
	})
	return u.steamcmdPath, u.steamcmdErr
}

// gameUser is the account the game unit runs as; steamcmd must run as it too
// or the updated files end up root-owned and the game cannot read them.
func (u *Updater) gameUser() string {
	type unitUserReader interface{ UnitUser() string }
	if u.sysd == nil {
		return ""
	}
	if r, ok := u.sysd.(unitUserReader); ok {
		return r.UnitUser()
	}
	return ""
}

// remoteBuildRe extracts the public branch buildid from app_info_print's VDF.
// Output shape (abridged):
//
//	"730"
//	{
//		...
//		"branches"
//		{
//			"public"
//			{
//				"buildid"		"16063831"
//				...
//			}
//		}
//	}
var remoteBuildRe = regexp.MustCompile(`(?s)"branches".*?"public".*?"buildid"\s+"(\d+)"`)

// runSteamcmd executes steamcmd as the game user with the given commands,
// through the run seam (tests substitute their own runner).
func (u *Updater) runSteamcmd(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	if u.run == nil {
		u.run = u.execSteamcmd
	}
	return u.run(ctx, timeout, args...)
}

// execSteamcmd is the real shell-out. The agent runs as root; steamcmd run
// as root writes root-owned files into the game tree, which the game user
// then cannot read — so the command is demoted exactly like bootstrap.sh
// does with sudo -u.
func (u *Updater) execSteamcmd(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	bin, err := u.SteamCMDPath()
	if err != nil {
		return "", err
	}
	user := u.gameUser()
	var runAs string
	var cmdArgs []string
	if user != "" && user != "root" {
		// Demote exactly like bootstrap.sh does. runuser keeps the
		// environment minimal, which steamcmd prefers, but a minimal install
		// may not ship it; su -c is the portable fallback.
		if _, err := exec.LookPath("runuser"); err == nil {
			runAs = "runuser"
			cmdArgs = []string{"-u", user, "--", bin}
		} else {
			runAs = "su"
			cmdArgs = []string{"-", user, "-c", bin + " " + strings.Join(args, " ")}
			args = nil // folded into the -c string above
		}
	} else {
		runAs = bin
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, runAs, append(cmdArgs, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+homeFor(user), "TERM=dumb")
	out, err := cmd.CombinedOutput()
	if cctx.Err() == context.DeadlineExceeded {
		return string(out), &UpdateCheckError{Reason: "steamcmd timed out"}
	}
	return string(out), err
}

// homeFor resolves a user's home directory for $HOME; root's default is fine.
func homeFor(user string) string {
	if user == "" || user == "root" {
		return "/root"
	}
	return "/home/" + user
}

// RemoteBuildID asks Steam what build app 730 is at on the public branch.
// The answer is cached: app_info requests are rate-limited by Steam, and the
// auto-check loop polls the cache between refreshes.
func (u *Updater) RemoteBuildID(ctx context.Context, force bool) (string, error) {
	u.mu.Lock()
	if !force && u.remoteBuild != "" && time.Since(u.remoteFetched) < remoteBuildCacheTTL {
		v := u.remoteBuild
		u.mu.Unlock()
		return v, nil
	}
	u.mu.Unlock()

	out, err := u.runSteamcmd(ctx, 90*time.Second,
		"+login", "anonymous", "+app_info_update", "1", "+app_info_print", strconv.Itoa(CS2AppID), "+quit")
	if err != nil && !strings.Contains(out, "buildid") {
		return "", &UpdateCheckError{Reason: steamcmdError(out, err)}
	}
	m := remoteBuildRe.FindStringSubmatch(out)
	if m == nil {
		return "", &UpdateCheckError{Reason: "steamcmd did not report a buildid for app 730 (rate limit? retry in a few minutes)"}
	}
	u.mu.Lock()
	u.remoteBuild = m[1]
	u.remoteFetched = time.Now()
	u.mu.Unlock()
	return m[1], nil
}

// steamcmdError trims steamcmd's log noise to the informative tail.
func steamcmdError(out string, err error) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		if err != nil {
			return err.Error()
		}
		return "steamcmd failed"
	}
	start := len(lines) - 5
	if start < 0 {
		start = 0
	}
	tail := strings.TrimSpace(strings.Join(lines[start:], "\n"))
	if err != nil && !strings.Contains(tail, "Error") {
		tail += " (" + err.Error() + ")"
	}
	return tail
}

// Check compares the installed build with Steam's, caching the remote answer
// unless force is set. It records state the status API reports.
func (u *Updater) Check(ctx context.Context, force bool) UpdateInfo {
	local, lerr := u.LocalBuildID()
	remote, rerr := u.RemoteBuildID(ctx, force)

	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	if lerr != nil || rerr != nil {
		u.lastErr = ""
		if lerr != nil {
			u.lastErr = lerr.Error()
		}
		if rerr != nil {
			u.lastErr = joinNotes(u.lastErr, rerr.Error())
		}
		u.lastCheck = now
		return u.snapshotLocked()
	}
	u.lastErr = ""
	u.lastCheck = now
	avail := local != remote
	if avail && u.availableSince.IsZero() {
		u.availableSince = now
	}
	if !avail {
		u.availableSince = time.Time{}
		u.pendingPlayers = false
	}
	return u.snapshotLocked()
}

// Info is the cached answer without touching Steam.
func (u *Updater) Info() UpdateInfo {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.snapshotLocked()
}

func (u *Updater) snapshotLocked() UpdateInfo {
	return UpdateInfo{
		InstalledBuild: u.cachedLocal(),
		LatestBuild:    u.remoteBuild,
		Available:      u.availableSince != time.Time{},
		AvailableSince: u.availableSince,
		PendingPlayers: u.pendingPlayers,
		Updating:       u.updating,
		AutoUpdate:     u.cfg.AutoUpdate,
		LastCheck:      u.lastCheck,
		LastError:      u.lastErr,
	}
}

// cachedLocal is read without the main lock path; the manifest is a file
// read, cheap enough to do on every snapshot for freshness after an update.
func (u *Updater) cachedLocal() string {
	v, err := u.LocalBuildID()
	if err != nil {
		return ""
	}
	return v
}

// SetUpdating flips the "an update job is running" flag shown in status.
func (u *Updater) SetUpdating(on bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.updating = on
}

// SetPendingPlayers records that an update is known but held for players.
func (u *Updater) SetPendingPlayers(on bool) {
	u.mu.Lock()
	u.pendingPlayers = on
	u.mu.Unlock()
}

// ClearAvailable resets the available state after a successful update, so
// the badge disappears instead of lingering until the next periodic check.
func (u *Updater) ClearAvailable() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.availableSince = time.Time{}
	u.pendingPlayers = false
	u.remoteBuild = u.cachedLocal()
	u.remoteFetched = time.Now()
	u.lastErr = ""
}

// recordCheckError notes a failed background check for the panel to see.
func (u *Updater) recordCheckError(reason string) {
	u.mu.Lock()
	u.lastErr = reason
	u.lastCheck = time.Now()
	u.mu.Unlock()
}

// Run applies the update: stop the game server if it is running, let
// steamcmd bring app 730 to Steam's current build, then restore the server
// to the state it was found in. progress reports human-readable steps; the
// whole thing runs inside an agent job, never an HTTP request.
func (u *Updater) Run(ctx context.Context, progress func(string)) (err error) {
	progress("checking for an update")
	local, err := u.LocalBuildID()
	if err != nil {
		return err
	}
	remote, err := u.RemoteBuildID(ctx, true)
	if err != nil {
		return err
	}
	if local == remote {
		progress("already up to date (build " + local + ")")
		u.ClearAvailable()
		return nil
	}

	wasActive := false
	if u.sysd != nil {
		if active, aerr := u.sysd.IsActive(); aerr == nil && active {
			wasActive = true
		}
	}
	u.wasActive = wasActive

	if wasActive {
		progress("stopping the game server")
		if err := u.sysd.Stop(); err != nil {
			return fmt.Errorf("stop the game server first: %w", err)
		}
	}

	progress("downloading update (steamcmd, build " + remote + ")")
	out, uerr := u.runSteamcmd(ctx, 40*time.Minute,
		"+force_install_dir", u.cfg.CS2Dir,
		"+login", "anonymous",
		"+app_update", strconv.Itoa(CS2AppID), "validate",
		"+quit")
	if uerr != nil {
		// steamcmd writes "Success! App '730' fully installed" even for a
		// partial no-op run; trust the manifest over the log line.
		progress("steamcmd finished with an error — verifying the install")
		after, aerr := u.LocalBuildID()
		if aerr != nil || after != remote {
			if wasActive {
				_ = u.sysd.Start()
			}
			return &UpdateCheckError{Reason: steamcmdError(out, uerr)}
		}
	}

	after, err := u.LocalBuildID()
	if err != nil {
		if wasActive {
			_ = u.sysd.Start()
		}
		return err
	}
	if after != remote {
		if wasActive {
			_ = u.sysd.Start()
		}
		return &UpdateCheckError{Reason: "steamcmd reported success but the build is still " + after + " (wanted " + remote + ")"}
	}

	u.ClearAvailable()
	if wasActive {
		progress("restarting the game server")
		if err := u.sysd.Start(); err != nil {
			return fmt.Errorf("updated to build %s, but the server did not start: %w", remote, err)
		}
	}
	progress("updated to build " + remote)
	return nil
}

// RunAutoLoop periodically checks for updates and, when configured for
// auto-update, applies them the moment the server is empty (or offline).
// It stops when ctx is cancelled and is intended to run for the agent's
// lifetime. Reports go to the status API (UpdateInfo), not to stdout: the
// panel is the operator's window into the agent.
func (u *Updater) RunAutoLoop(ctx context.Context) {
	if u.cfg.CS2Dir == "" {
		return
	}
	timer := time.NewTimer(firstUpdateCheckDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		u.autoPass(ctx)
		timer.Reset(updateCheckInterval)
	}
}

// autoPass is one iteration of the loop: check, and maybe update.
func (u *Updater) autoPass(ctx context.Context) {
	// If an update job is already running (started from the panel), nothing
	// to do this round.
	if u.Updating() {
		return
	}
	info := u.Check(ctx, false)
	if !info.Available {
		return
	}
	if !u.cfg.AutoUpdate {
		return // the panel's button is the only path
	}
	players := 0
	if u.humanPlayers != nil {
		players = u.humanPlayers()
	}
	if players > 0 {
		// Never kick anyone: the update waits until the server empties.
		u.SetPendingPlayers(true)
		return
	}
	// Server empty or offline — safe to update now. The panel's own Update
	// button runs the same Run through a job; here it runs inline in the
	// loop goroutine, which is just as backgrounded.
	u.SetPendingPlayers(false)
	u.SetUpdating(true)
	err := u.Run(ctx, func(step string) {})
	u.SetUpdating(false)
	if err != nil {
		u.recordCheckError(err.Error())
	}
}

// Updating reports whether an update job is in flight.
func (u *Updater) Updating() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.updating
}
