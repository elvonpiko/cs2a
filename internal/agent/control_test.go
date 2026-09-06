package agent

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// crashLoopService is a fakeService that also reports the richer health
// surface, letting Status and Control observe a unit systemd keeps
// restarting. It embeds the pointer (not the value) so the fake's mutex is
// never copied.
type crashLoopService struct {
	*fakeService
	health UnitHealthState
	reset  bool
}

func (f *crashLoopService) UnitHealth() UnitHealthState { return f.health }
func (f *crashLoopService) ResetFailed() error          { f.reset = true; return nil }

// ActiveState mirrors what systemd reports: "activating" during the
// auto-restart gap, so readState sees a unit that is not really up. The plain
// fakeService answers is-active only, which cannot express that.
func (f *crashLoopService) ActiveState() (string, string, string) {
	return f.health.ActiveState, f.health.SubState, f.health.Result
}

// newCrashLoopServer wires a Server around a health-reporting fake.
func newCrashLoopServer(t *testing.T) (*Server, *crashLoopService) {
	t.Helper()
	srv, svc, _, _ := newTestServer(t, nil)
	// Swap the plain fake for the health-aware one, keeping the wiring.
	h := &crashLoopService{fakeService: svc}
	h.mu.Lock()
	h.active = false
	h.mu.Unlock()
	srv.sysd = h
	return srv, h
}

// The bug from the first real deploy: a CS2 install with no steamclient.so
// segfaults on boot, systemd restarts it every few seconds, and Status kept
// answering "active" during the seconds the process was alive. The panel
// flipped between "Running" and "Offline" forever and blamed a slow map load.
func TestStatusReportsCrashLoop(t *testing.T) {
	srv, svc := newCrashLoopServer(t)
	now := time.Now()
	svc.health = UnitHealthState{
		ActiveState: "activating", SubState: "auto-restart",
		NRestarts: 5, ExecMainCode: "killed", ExecMainStatus: 11,
		InactiveEnterTimestamp: "@" + strconv.FormatInt(now.Add(-5*time.Second).Unix(), 10),
	}

	st := srv.Status(t.Context())
	if st.Service.Active {
		t.Fatalf("crash-looping unit reported as active: %+v", st.Service)
	}
	if !st.Service.CrashLooping {
		t.Fatalf("CrashLooping not set: %+v", st.Service)
	}
	if st.Service.RestartCount != 5 {
		t.Fatalf("RestartCount = %d, want 5", st.Service.RestartCount)
	}
	if st.Service.ExitCodeKind != "killed" || st.Service.ExitCode != 11 {
		t.Fatalf("exit code not surfaced: %+v", st.Service)
	}
	if !strings.Contains(st.Note, "keeps crashing") {
		t.Fatalf("note = %q", st.Note)
	}
}

// A healthy server that has collected a few restarts over weeks must not read
// as a crash loop: NRestarts is cumulative, so the recent-death check is what
// keeps it honest.
func TestStatusIgnoresStaleRestarts(t *testing.T) {
	srv, svc := newCrashLoopServer(t)
	svc.mu.Lock()
	svc.active = true
	svc.mu.Unlock()
	svc.health = UnitHealthState{
		ActiveState: "active", SubState: "running",
		NRestarts:              7,
		InactiveEnterTimestamp: "@" + strconv.FormatInt(time.Now().Add(-72*time.Hour).Unix(), 10),
	}

	st := srv.Status(t.Context())
	if st.Service.CrashLooping {
		t.Fatalf("weeks-old restarts read as a crash loop: %+v", st.Service)
	}
	if !st.Service.Active {
		t.Fatalf("healthy server reported inactive: %+v", st.Service)
	}
}

// Control must not report a start that lands inside a crash loop as success,
// even for the seconds the process lives.
func TestControlDetectsCrashLoopAfterStart(t *testing.T) {
	srv, svc := newCrashLoopServer(t)
	now := time.Now()
	svc.health = UnitHealthState{
		ActiveState: "activating", SubState: "auto-restart",
		NRestarts: 4, ExecMainCode: "killed", ExecMainStatus: 11,
		InactiveEnterTimestamp: "@" + strconv.FormatInt(now.Add(-10*time.Second).Unix(), 10),
	}
	svc.mu.Lock()
	svc.active = false // the doomed process is between restarts
	svc.mu.Unlock()

	res := srv.Control(t.Context(), ActionStart)
	if !res.Failed {
		t.Fatalf("start inside a crash loop reported as success: %+v", res)
	}
	if res.Active {
		t.Fatalf("crash-looping unit reported active: %+v", res)
	}
	if !strings.Contains(res.Message, "crashing on startup") {
		t.Fatalf("message = %q", res.Message)
	}
	if !strings.Contains(res.Message, "segmentation fault") {
		t.Fatalf("SIGSEGV not explained: %q", res.Message)
	}
	if len(res.Log) == 0 {
		t.Fatalf("journal tail missing from a crash-loop report")
	}
}

// The start limit refuses further starts ("start request repeated too
// quickly") until reset-failed clears it; Control runs it before every
// start/restart so the panel's Start button is the recovery path.
func TestControlResetsStartLimitBeforeStart(t *testing.T) {
	srv, svc := newCrashLoopServer(t)
	svc.mu.Lock()
	svc.active = true
	svc.mu.Unlock()
	svc.health = UnitHealthState{ActiveState: "active", SubState: "running"}

	_ = srv.Control(t.Context(), ActionStart)
	if !svc.reset {
		t.Fatalf("reset-failed was not issued before start")
	}
	svc.reset = false
	_ = srv.Control(t.Context(), ActionRestart)
	if !svc.reset {
		t.Fatalf("reset-failed was not issued before restart")
	}
	svc.reset = false
	_ = srv.Control(t.Context(), ActionStop)
	if svc.reset {
		t.Fatalf("reset-failed issued before a stop, where it changes nothing")
	}
}

func TestCrashLoopMessage(t *testing.T) {
	for _, tc := range []struct {
		h    UnitHealthState
		want string
	}{
		{UnitHealthState{NRestarts: 5, ExecMainCode: "killed", ExecMainStatus: 11}, "segmentation fault"},
		{UnitHealthState{NRestarts: 5, ExecMainCode: "killed", ExecMainStatus: 6}, "signal 6"},
		{UnitHealthState{NRestarts: 5, ExecMainCode: "exited", ExecMainStatus: 1}, "status 1"},
		{UnitHealthState{NRestarts: 5}, "restarted it 5 times"},
	} {
		if got := crashLoopMessage(tc.h); !strings.Contains(got, tc.want) {
			t.Fatalf("crashLoopMessage(%+v) = %q, want %q inside", tc.h, got, tc.want)
		}
	}
}
