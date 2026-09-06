package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Job states.
const (
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// Job is one long-running agent operation (plugin install/uninstall) tracked
// out-of-band so the panel never has to hold an HTTP request open for minutes.
type Job struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`   // "install" | "uninstall"
	Target   string         `json:"target"` // catalog id
	Label    string         `json:"label"`  // human-readable component name
	Status   string         `json:"status"`
	Step     string         `json:"step,omitempty"`    // human-readable progress
	Message  string         `json:"message,omitempty"` // error text when failed
	Result   *InstallResult `json:"result,omitempty"`
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished,omitempty"`
	// DownloadBytes/DownloadTotal are the live progress of the artifact
	// download, the phase that dominates an install on a slow link. Total
	// 0 means the size is unknown (chunked transfer) and the panel shows
	// an indeterminate bar.
	DownloadBytes int64 `json:"download_bytes,omitempty"`
	DownloadTotal int64 `json:"download_total,omitempty"`
	// RestartObserved (filled by the API layer, not the job itself) says the
	// game server has (re)started since this install finished, so the
	// "restart the server to load it" hint is stale and must not be shown.
	RestartObserved bool `json:"restart_observed,omitempty"`

	// lastByteReport throttles byte-counter updates; runtime state only.
	lastByteReport time.Time
}

// Elapsed is how long the job ran (or has been running).
func (j Job) Elapsed() time.Duration {
	if j.Finished.IsZero() {
		return time.Since(j.Started)
	}
	return j.Finished.Sub(j.Started)
}

// Jobs is an in-memory job registry. Finished jobs are retained briefly so the
// panel can report the outcome, then reaped.
type Jobs struct {
	mu   sync.Mutex
	jobs map[string]*Job
	// Retain is how long a finished job stays queryable.
	Retain time.Duration
}

// NewJobs builds an empty registry.
func NewJobs() *Jobs {
	return &Jobs{jobs: map[string]*Job{}, Retain: 10 * time.Minute}
}

// ErrBusy is returned when the same target already has a running job.
type ErrBusy struct {
	Target string
	Kind   string
}

func (e *ErrBusy) Error() string {
	return fmt.Sprintf("%s is already being %sed", e.Target, e.Kind)
}

// Progress is one update from a running job: a human-readable step and, while
// an artifact is downloading, the live byte count.
type Progress struct {
	Step string
	// DownloadBytes/DownloadTotal are set (>0 total) only during the download
	// step, so the panel can draw a byte-accurate bar.
	DownloadBytes int64
	DownloadTotal int64
}

// Start registers a job for target and runs fn in the background. Only one
// job per target may run at a time; a second attempt returns *ErrBusy.
//
// fn receives a progress callback and a context that outlives the HTTP request
// that started it.
func (js *Jobs) Start(kind, target, label string, fn func(ctx context.Context, progress func(Progress)) (*InstallResult, error)) (*Job, error) {
	js.mu.Lock()
	js.reapLocked()
	for _, j := range js.jobs {
		if j.Target == target && j.Status == JobRunning {
			js.mu.Unlock()
			return nil, &ErrBusy{Target: target, Kind: j.Kind}
		}
	}
	if label == "" {
		label = target
	}
	job := &Job{
		ID:      newJobID(),
		Kind:    kind,
		Target:  target,
		Label:   label,
		Status:  JobRunning,
		Step:    "starting",
		Started: time.Now(),
	}
	js.jobs[job.ID] = job
	js.mu.Unlock()

	go func() {
		// The job must survive the request that started it, but not forever.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		res, err := fn(ctx, func(p Progress) { js.report(job.ID, p) })
		js.mu.Lock()
		defer js.mu.Unlock()
		job.Finished = time.Now()
		if err != nil {
			job.Status = JobFailed
			job.Message = err.Error()
			job.Step = ""
			job.DownloadBytes, job.DownloadTotal = 0, 0
			return
		}
		job.Status = JobDone
		job.Step = ""
		job.DownloadBytes, job.DownloadTotal = 0, 0
		job.Result = res
	}()
	return job.snapshot(js), nil
}

// report records one progress update. Byte counters are throttled to one
// update per 100ms — a 50 MB download on a fast link copies in bursts that
// would otherwise lock the registry mutex thousands of times a second, while
// the panel polls at 1s anyway.
func (js *Jobs) report(id string, p Progress) {
	js.mu.Lock()
	defer js.mu.Unlock()
	j, ok := js.jobs[id]
	if !ok || j.Status != JobRunning {
		return
	}
	if p.DownloadTotal > 0 {
		now := time.Now()
		if !j.lastByteReport.IsZero() && now.Sub(j.lastByteReport) < 100*time.Millisecond {
			// Keep the freshest byte count anyway so the final update is
			// never a stale one.
			j.DownloadBytes, j.DownloadTotal = p.DownloadBytes, p.DownloadTotal
			return
		}
		j.lastByteReport = now
	}
	j.Step = p.Step
	if p.Step == "" || p.DownloadTotal == 0 {
		j.DownloadBytes, j.DownloadTotal = 0, 0
	}
	if p.DownloadTotal > 0 {
		j.DownloadBytes, j.DownloadTotal = p.DownloadBytes, p.DownloadTotal
	}
}

// Get returns a copy of one job.
func (js *Jobs) Get(id string) (Job, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	j, ok := js.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// List returns copies of all retained jobs, newest first.
func (js *Jobs) List() []Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.reapLocked()
	out := make([]Job, 0, len(js.jobs))
	for _, j := range js.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Started.After(out[k].Started) })
	return out
}

// RunningTargets lists the catalog ids that currently have a job in flight.
// Callers use it to refuse work that would fight a running install: an
// uninstall that deletes files an extraction is still writing leaves a
// half-installed plugin whose recorded manifest no longer matches the disk.
func (js *Jobs) RunningTargets() []string {
	js.mu.Lock()
	defer js.mu.Unlock()
	var out []string
	for _, j := range js.jobs {
		if j.Status == JobRunning {
			out = append(out, j.Target)
		}
	}
	sort.Strings(out)
	return out
}

// snapshot copies a job under the registry lock.
func (j *Job) snapshot(js *Jobs) *Job {
	js.mu.Lock()
	defer js.mu.Unlock()
	cp := *j
	return &cp
}

// reapLocked drops finished jobs older than Retain. Caller holds the lock.
func (js *Jobs) reapLocked() {
	retain := js.Retain
	if retain <= 0 {
		retain = 10 * time.Minute
	}
	for id, j := range js.jobs {
		if j.Status != JobRunning && time.Since(j.Finished) > retain {
			delete(js.jobs, id)
		}
	}
}

func newJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
