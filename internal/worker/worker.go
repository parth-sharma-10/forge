// Package worker is the job loop: claim a job, run it in the sandbox while
// keeping its lease alive, report what happened, repeat.
//
// Two stops, as with the pool: drain finishes the job in hand and claims no
// more; cancel kills the job in hand. They are different signals because an
// operator restarting a worker wants the first and an operator whose worker
// is wedged wants the second.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/parth-sharma-10/forge/internal/cgroup"
	"github.com/parth-sharma-10/forge/internal/metrics"
	"github.com/parth-sharma-10/forge/internal/queue"
	"github.com/parth-sharma-10/forge/internal/sandbox"
)

// Store is what the loop needs from the queue. *queue.Queue is the one that
// matters; the tests supply one in memory to drive the loop without a
// database.
type Store interface {
	Claim(ctx context.Context, worker string, lease time.Duration) (queue.Job, error)
	Heartbeat(ctx context.Context, job queue.Job, lease time.Duration, log queue.Log) error
	Report(ctx context.Context, job queue.Job, r queue.Result) error
}

// Worker runs jobs from Store. Every field but Store and Rootfs has a
// working zero value.
type Worker struct {
	Store  Store
	Name   string // recorded on the job as who claimed it
	Rootfs string
	Limits cgroup.Limits
	// CgroupRoot is passed through to the sandbox; empty means the default.
	CgroupRoot string

	// Lease is how long a claim holds without a heartbeat. Heartbeats go
	// out every third of it or every two seconds, whichever is sooner —
	// each one carries the log so far, and that is how stale a followed
	// log can be. Poll is the sleep after an empty claim.
	Lease, Poll time.Duration
	// LogLimit is how many bytes of the build's output are kept — the tail,
	// since the end of a build log is where the reason it failed is.
	LogLimit int
	// MaxAttempts is how many times a job may be claimed before a worker
	// handed it reports it as given up instead of running it. A job that
	// kills every worker it lands on would otherwise land forever.
	MaxAttempts int

	// Run executes one job. It is sandbox.Run unless a test says otherwise.
	Run func(ctx context.Context, spec sandbox.Spec) error
	Log *slog.Logger
	// Metrics is where the counters are registered; nil means a registry
	// of the worker's own that nothing scrapes.
	Metrics *metrics.Registry

	claimed, leaseLost *metrics.Counter
	done               map[string]*metrics.Counter
	running            *metrics.Gauge
}

// outcomes are the labels of forge_jobs_done_total.
var outcomes = []string{"ok", "failed", "oom", "killed", "setup", "given_up"}

const (
	defaultLease       = 30 * time.Second
	defaultPoll        = time.Second
	defaultLogLimit    = 1 << 20
	defaultMaxAttempts = 3
	maxFlush           = 2 * time.Second
)

func (w *Worker) heartbeatEvery() time.Duration {
	return min(w.Lease/3, maxFlush)
}

func (w *Worker) defaults() {
	if w.Lease == 0 {
		w.Lease = defaultLease
	}
	if w.Poll == 0 {
		w.Poll = defaultPoll
	}
	if w.LogLimit == 0 {
		w.LogLimit = defaultLogLimit
	}
	if w.MaxAttempts == 0 {
		w.MaxAttempts = defaultMaxAttempts
	}
	if w.Run == nil {
		w.Run = sandbox.Run
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	if w.Metrics == nil {
		w.Metrics = &metrics.Registry{}
	}
	w.claimed = w.Metrics.Counter("forge_jobs_claimed_total", "Jobs claimed from the queue.", nil)
	w.leaseLost = w.Metrics.Counter("forge_lease_lost_total", "Heartbeats refused: the job had been reclaimed.", nil)
	w.running = w.Metrics.Gauge("forge_jobs_running", "Jobs in hand right now.", nil)
	w.done = map[string]*metrics.Counter{}
	for _, o := range outcomes {
		w.done[o] = w.Metrics.Counter("forge_jobs_done_total", "Jobs reported, by outcome.", map[string]string{"outcome": o})
	}
}

// Serve runs n loops until stop is closed or ctx is cancelled, and returns
// once every loop has. Closing stop is the drain: each loop finishes the
// job it holds and claims no more. Cancelling ctx is the kill: the job in
// hand gets its context cancelled, which the sandbox turns into SIGKILL of
// its PID 1 and everything under it.
func (w *Worker) Serve(ctx context.Context, stop <-chan struct{}, n int) {
	w.defaults()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx, stop)
		}()
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		default:
		}
		job, err := w.Store.Claim(ctx, w.Name, w.Lease)
		switch {
		case errors.Is(err, queue.ErrEmpty):
			select {
			case <-time.After(w.Poll):
			case <-stop:
			case <-ctx.Done():
			}
			continue
		case err != nil:
			// The database, not the job: wait out a blip rather than spin.
			w.Log.Error("claim failed", "err", err)
			select {
			case <-time.After(w.Poll):
			case <-stop:
			case <-ctx.Done():
			}
			continue
		}
		w.run(ctx, job)
	}
}

// run executes one claimed job and reports it.
//
// The heartbeat runs beside the build and cancels it on a lost lease: once
// another worker holds the job, this build's result can never be reported,
// so the only thing left to do with it is stop paying for it. Each
// heartbeat carries the log so far, which is what lets a reader follow a
// build while it runs. The report itself goes out on ctx, not the build's
// context — a build cancelled for a lost lease still tries to report, and
// finds out it cannot.
func (w *Worker) run(ctx context.Context, job queue.Job) {
	w.claimed.Inc()
	// Enforced here rather than in the claim query: a job the query refused
	// to hand out would sit running with an expired lease, a row nothing
	// touches again. Handed out and reported, it is done, and says why.
	if job.Attempt > w.MaxAttempts {
		r := queue.Result{ExitCode: -1, Error: fmt.Sprintf("given up: attempt %d, limit %d", job.Attempt, w.MaxAttempts)}
		if err := w.Store.Report(ctx, job, r); err != nil {
			w.Log.Warn("report failed", "job", job.ID, "err", err)
		}
		w.done["given_up"].Inc()
		w.Log.Warn("job given up", "job", job.ID, "attempt", job.Attempt)
		return
	}

	w.running.Inc()
	defer w.running.Dec()
	buildCtx, cancelBuild := context.WithCancel(ctx)
	defer cancelBuild()

	log := &tail{limit: w.LogLimit}
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		t := time.NewTicker(w.heartbeatEvery())
		defer t.Stop()
		for {
			select {
			case <-buildCtx.Done():
				return
			case <-t.C:
			}
			if err := w.Store.Heartbeat(ctx, job, w.Lease, log.Log()); err != nil {
				if errors.Is(err, queue.ErrLeaseLost) {
					w.leaseLost.Inc()
				}
				w.Log.Warn("heartbeat failed, killing the build", "job", job.ID, "err", err)
				cancelBuild()
				return
			}
		}
	}()

	err := w.Run(buildCtx, sandbox.Spec{
		Rootfs:     w.Rootfs,
		Args:       job.Args,
		Limits:     w.Limits,
		CgroupRoot: w.CgroupRoot,
		Stdout:     log,
		Stderr:     log,
	})
	cancelBuild()
	<-heartbeatDone

	r := result(err)
	r.Log = log.Log()
	w.done[outcome(r)].Inc()
	if err := w.Store.Report(ctx, job, r); err != nil {
		w.Log.Warn("report failed", "job", job.ID, "err", err)
		return
	}
	w.Log.Info("job done", "job", job.ID, "exit", r.ExitCode, "oom", r.OOMKilled, "error", r.Error)
}

// outcome names a result for the counter.
func outcome(r queue.Result) string {
	switch {
	case r.OOMKilled:
		return "oom"
	case r.ExitCode == sandbox.SetupExitCode:
		return "setup"
	case r.ExitCode == -1:
		return "killed"
	case r.ExitCode == 0:
		return "ok"
	}
	return "failed"
}

// result turns what the sandbox returned into what the queue records.
func result(err error) queue.Result {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return queue.Result{ExitCode: 0}
	case errors.As(err, &exitErr):
		// A signal — an OOM kill, a lease lost, an operator's kill -9 — has
		// no exit status, and ExitCode reports it as -1. OOMKilled is the
		// reason when there is one; Error is only for the ones without.
		r := queue.Result{ExitCode: exitErr.ExitCode(), OOMKilled: errors.Is(err, sandbox.ErrOOMKilled)}
		switch {
		case r.ExitCode == sandbox.SetupExitCode:
			r.Error = "the sandbox could not be built; see the log"
		case r.ExitCode == -1 && !r.OOMKilled:
			r.Error = fmt.Sprintf("killed: %v", err)
		}
		return r
	default:
		return queue.Result{ExitCode: -1, Error: err.Error()}
	}
}

// tail keeps the last limit bytes written to it, and counts them all.
//
// ponytail: a copy on every overflow rather than a ring, so a build that
// writes far more than the limit pays O(n) per write past it. Trade for a
// ring buffer when a log-heavy build shows up in a profile.
type tail struct {
	mu    sync.Mutex
	buf   []byte
	total int64
	limit int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	t.total += int64(len(p))
	if len(t.buf) > t.limit {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.limit:]...)
	}
	return len(p), nil
}

func (t *tail) Log() queue.Log {
	t.mu.Lock()
	defer t.mu.Unlock()
	return queue.Log{Tail: append([]byte(nil), t.buf...), Total: t.total}
}
