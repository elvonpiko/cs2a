package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeUpdaterService is a ServiceController stand-in that records what the
// updater did to the game unit.
type fakeUpdaterService struct {
	active  bool
	actions []string
	user    string // UnitUser answer; "" means root (run steamcmd bare)
}

func (f *fakeUpdaterService) Start() error {
	f.actions = append(f.actions, "start")
	f.active = true
	return nil
}
func (f *fakeUpdaterService) Stop() error {
	f.actions = append(f.actions, "stop")
	f.active = false
	return nil
}
func (f *fakeUpdaterService) Restart() error {
	f.actions = append(f.actions, "restart")
	f.active = true
	return nil
}
func (f *fakeUpdaterService) IsActive() (bool, error)        { return f.active, nil }
func (f *fakeUpdaterService) IsEnabled() (bool, error)       { return true, nil }
func (f *fakeUpdaterService) UptimeSeconds() (float64, bool) { return 0, false }
func (f *fakeUpdaterService) UnitUser() string               { return f.user }

// manifestFor writes an appmanifest_730.acf like steamcmd's.
func manifestFor(t *testing.T, dir, build string) {
	t.Helper()
	acf := `"AppState"
{
	"appid"      "730"
	"buildid"    "` + build + `"
}`
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "appmanifest_730.acf"), []byte(acf), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLocalBuildID reads the buildid out of the app manifest.
func TestLocalBuildID(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "1234567")
	u := NewUpdater(Config{CS2Dir: dir}, nil, nil)
	got, err := u.LocalBuildID()
	if err != nil || got != "1234567" {
		t.Fatalf("LocalBuildID = %q, %v", got, err)
	}

	// missing manifest → a check error, not a crash
	u2 := NewUpdater(Config{CS2Dir: t.TempDir()}, nil, nil)
	_, err = u2.LocalBuildID()
	if err == nil {
		t.Fatal("missing manifest must be an error")
	}
	var ce *UpdateCheckError
	if !errors.As(err, &ce) {
		t.Fatalf("expected UpdateCheckError, got %T", err)
	}
}

// remoteBuildFor points the updater at a fake "steamcmd" on PATH that prints
// an app_info answer with the given buildid.
func remoteBuildFor(t *testing.T, build string) (dir string, restore func()) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "steamcmd")
	script := "#!/bin/sh\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in\n" +
		"    +app_info_print) shift; cat <<EOF\n" +
		"\"730\"\n" +
		"{\n" +
		"	\"common\" { \"name\" \"Counter-Strike 2\" }\n" +
		"	\"branches\"\n" +
		"	{\n" +
		"		\"public\"\n" +
		"		{\n" +
		"			\"buildid\"		\"" + build + "\"\n" +
		"		}\n" +
		"	}\n" +
		"}\n" +
		"EOF\n" +
		"		exit 0 ;;\n" +
		"    +app_update) shift; echo \"Success! App '730' fully installed.\"; exit 0 ;;\n" +
		"  esac\n" +
		"  shift\n" +
		"done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", filepath.Dir(bin)+":"+oldPath)
	return bin, func() { os.Setenv("PATH", oldPath) }
}

// newTestUpdater wires an updater against a fake service. steamcmd is
// resolved through PATH, which the tests point at a fake binary.
func newTestUpdater(t *testing.T, cfg Config, svc *fakeUpdaterService) *Updater {
	t.Helper()
	return NewUpdater(cfg, svc, nil)
}

func TestCheckDetectsUpdateAndItsAbsence(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "100")
	_, restore := remoteBuildFor(t, "200")
	defer restore()

	u := newTestUpdater(t, Config{CS2Dir: dir}, &fakeUpdaterService{})
	info := u.Check(context.Background(), true)
	if info.LastError != "" {
		t.Fatalf("check error: %s", info.LastError)
	}
	if !info.Available {
		t.Fatalf("100 vs 200 must be available: %+v", info)
	}
	if info.InstalledBuild != "100" || info.LatestBuild != "200" {
		t.Fatalf("builds = %q -> %q", info.InstalledBuild, info.LatestBuild)
	}

	// same build → not available
	manifestFor(t, dir, "200")
	info = u.Check(context.Background(), true)
	if info.Available {
		t.Fatal("200 vs 200 must not be available")
	}
}

// TestCheckReportsSteamFailure: a steamcmd that answers nothing must surface
// as LastError, not as "no update".
func TestCheckReportsSteamFailure(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "100")
	// fake steamcmd that exits 1 with nothing useful
	bin := filepath.Join(t.TempDir(), "steamcmd")
	os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", filepath.Dir(bin)+":"+oldPath)
	defer os.Setenv("PATH", oldPath)

	u := newTestUpdater(t, Config{CS2Dir: dir}, &fakeUpdaterService{})
	info := u.Check(context.Background(), true)
	if info.LastError == "" {
		t.Fatal("a dead steamcmd must be reported")
	}
	if info.Available {
		t.Fatal("no comparison was possible; Available must be false")
	}
}

// TestRunUpdatesAndRestoresService: with the server online, an update stops
// it, applies, and starts it again; with the server offline it stays off.
func TestRunUpdatesAndRestoresService(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "100")
	_, restore := remoteBuildFor(t, "200")
	defer restore()

	// online: stop → update (rewrites the manifest like steamcmd would) → start
	svc := &fakeUpdaterService{active: true}
	u := newTestUpdater(t, Config{CS2Dir: dir}, svc)
	// The fake steamcmd "installs" by rewriting the manifest — the shell
	// stub can only answer app_info; the app_update rewrite is simulated
	// at the runner seam.
	u.run = func(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
		for _, a := range args {
			if a == "+app_update" {
				manifestFor(t, dir, "200")
			}
			if a == "+app_info_print" {
				return `"730"
{
	"branches"
	{
		"public"
		{
			"buildid"		"200"
		}
	}
}`, nil
			}
		}
		return "Success! App '730' fully installed.", nil
	}
	steps := []string{}
	err := u.Run(context.Background(), func(s string) { steps = append(steps, s) })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"stop", "start"}
	if len(svc.actions) != 2 || svc.actions[0] != want[0] || svc.actions[1] != want[1] {
		t.Fatalf("service actions = %v, want %v", svc.actions, want)
	}
	if got, _ := u.LocalBuildID(); got != "200" {
		t.Fatalf("build after update = %s", got)
	}
	if u.Info().Available {
		t.Fatal("after a successful update the badge must clear")
	}

	// offline: no service calls at all
	manifestFor(t, dir, "300")
	svc2 := &fakeUpdaterService{active: false}
	u2 := newTestUpdater(t, Config{CS2Dir: dir}, svc2)
	u2.run = func(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
		for _, a := range args {
			if a == "+app_update" {
				manifestFor(t, dir, "400")
			}
			if a == "+app_info_print" {
				return `"730"
{
	"branches"
	{
		"public"
		{
			"buildid"		"400"
		}
	}
}`, nil
			}
		}
		return "Success!", nil
	}
	if err := u2.Run(context.Background(), func(string) {}); err != nil {
		t.Fatalf("Run offline: %v", err)
	}
	if len(svc2.actions) != 0 {
		t.Fatalf("an offline server must stay untouched, actions = %v", svc2.actions)
	}
}

// TestRunNoopWhenCurrent: an up-to-date build is a no-op, not a restart.
func TestRunNoopWhenCurrent(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "200")
	_, restore := remoteBuildFor(t, "200")
	defer restore()

	svc := &fakeUpdaterService{active: true}
	u := newTestUpdater(t, Config{CS2Dir: dir}, svc)
	err := u.Run(context.Background(), func(string) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(svc.actions) != 0 {
		t.Fatalf("no update needed, yet service was touched: %v", svc.actions)
	}
}

// TestRunRefusesWithoutSteamcmd: no steamcmd on the box → a clear check
// error mentioning how to fix it.
func TestRunRefusesWithoutSteamcmd(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "100")
	oldPath := os.Getenv("PATH")
	os.Setenv("PATH", t.TempDir())
	defer os.Setenv("PATH", oldPath)

	u := NewUpdater(Config{CS2Dir: dir}, &fakeUpdaterService{}, nil)
	err := u.Run(context.Background(), func(string) {})
	if err == nil {
		t.Fatal("Run must fail without steamcmd")
	}
	if !strings.Contains(err.Error(), "steamcmd") {
		t.Fatalf("error should point at steamcmd: %v", err)
	}
}

// TestAutoPassWaitsForPlayers: with players online and auto-update on, the
// update is held (pending), not applied; with zero players it applies.
func TestAutoPassWaitsForPlayers(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "100")
	_, restore := remoteBuildFor(t, "200")
	defer restore()

	players := 3
	svc := &fakeUpdaterService{active: true}
	u := newTestUpdater(t, Config{CS2Dir: dir, AutoUpdate: true}, svc)
	u.humanPlayers = func() int { return players }
	u.run = func(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
		for _, a := range args {
			if a == "+app_update" {
				manifestFor(t, dir, "200")
			}
			if a == "+app_info_print" {
				return `"730"
{
	"branches"
	{
		"public"
		{
			"buildid"		"200"
		}
	}
}`, nil
			}
		}
		return "Success!", nil
	}

	u.autoPass(context.Background())
	if !u.Info().PendingPlayers {
		t.Fatal("with players online the update must be pending, not applied")
	}
	if got, _ := u.LocalBuildID(); got != "100" {
		t.Fatalf("update applied while players were online (build=%s)", got)
	}
	if len(svc.actions) != 0 {
		t.Fatalf("service must not be touched while players are online: %v", svc.actions)
	}

	players = 0
	u.autoPass(context.Background())
	if u.Info().PendingPlayers {
		t.Fatal("with no players the update must apply, not stay pending")
	}
	if got, _ := u.LocalBuildID(); got != "200" {
		t.Fatalf("update did not apply on the empty server (build=%s)", got)
	}
}

// TestAutoPassRespectsOptOut: auto_update=false never applies anything.
func TestAutoPassRespectsOptOut(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "100")
	_, restore := remoteBuildFor(t, "200")
	defer restore()

	svc := &fakeUpdaterService{active: true}
	u := newTestUpdater(t, Config{CS2Dir: dir, AutoUpdate: false}, svc)
	u.humanPlayers = func() int { return 0 }
	u.autoPass(context.Background())
	if got, _ := u.LocalBuildID(); got != "100" {
		t.Fatal("auto_update=false must not touch the install")
	}
	if len(svc.actions) != 0 {
		t.Fatalf("service touched despite opt-out: %v", svc.actions)
	}
}

// TestSteamcmdRunsAsGameUser: when the game unit runs as a real user, the
// steamcmd invocation is demoted through runuser; as root it runs bare.
func TestSteamcmdRunsAsGameUser(t *testing.T) {
	dir := t.TempDir()
	manifestFor(t, dir, "100")
	_, restore := remoteBuildFor(t, "200")
	defer restore()

	// a fake service whose user exists on every Linux test box: root
	svc := &fakeUpdaterService{active: false}
	u := newTestUpdater(t, Config{CS2Dir: dir}, svc)
	// capture the argv the real runner builds by pointing HOME elsewhere
	// and letting it actually run: the remoteBuildFor stub exits 0 without
	// touching anything, so this only proves the exec path works as root.
	if _, err := u.runSteamcmd(context.Background(), 5*time.Second,
		"+login", "anonymous", "+app_info_print", "730", "+quit"); err != nil {
		t.Fatalf("bare run as root failed: %v", err)
	}

	// a user that does not exist must surface as an error, not a silent
	// wrong-user update
	svcBad := &fakeUpdaterService{active: false}
	svcBad.user = "definitely-not-a-user-xyz"
	u2 := newTestUpdater(t, Config{CS2Dir: dir}, svcBad)
	_, err := u2.RemoteBuildID(context.Background(), true)
	if err == nil {
		t.Fatal("runuser with a nonexistent user must fail")
	}
}

// TestConfigAutoUpdateDefaultsOn: a config file written before auto_update
// existed reads as enabled, an explicit false stays false.
func TestConfigAutoUpdateDefaultsOn(t *testing.T) {
	dir := t.TempDir()

	// legacy file: no auto_update key
	legacy := filepath.Join(dir, "legacy.json")
	os.WriteFile(legacy, []byte(`{"token":"t","cs2_dir":"`+dir+`"}`), 0o600)
	cfg, err := LoadConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AutoUpdate {
		t.Fatal("a config predating auto_update must default it on")
	}

	// explicit opt-out survives
	optOut := filepath.Join(dir, "optout.json")
	os.WriteFile(optOut, []byte(`{"token":"t","cs2_dir":"`+dir+`","auto_update":false}`), 0o600)
	cfg2, err := LoadConfig(optOut)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.AutoUpdate {
		t.Fatal("an explicit auto_update=false must stay off")
	}
}
