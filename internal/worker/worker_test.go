package worker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parth-sharma-10/forge/internal/metrics"
	"github.com/parth-sharma-10/forge/internal/queue"
	"github.com/parth-sharma-10/forge/internal/sandbox"
)

// fakeStore is a queue in memory with the same contract: one claim per job,
// fenced writes. It exists to drive the loop; the queue's own tests are the
// ones that prove the contract against PostgreSQL.
type fakeStore struct {
	mu       sync.Mutex
	jobs     []queue.Job
	lostAt   int // heartbeats before the lease is reported lost; 0 = never
	cancelAt int // heartbeats before the job is reported cancelled; 0 = never
	beats    int
	flushed  []queue.Log // the log carried by each heartbeat
	reports  []queue.Result
	reported chan struct{}
}

func newStore(args ...[]string) *fakeStore {
	s := &fakeStore{reported: make(chan struct{}, 16)}
	for i, a := range args {
		s.jobs = append(s.jobs, queue.Job{ID: int64(i + 1), Args: a, Fence: 1})
	}
	return s
}

func (s *fakeStore) Claim(context.Context, string, time.Duration) (queue.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) == 0 {
		return queue.Job{}, queue.ErrEmpty
	}
	j := s.jobs[0]
	s.jobs = s.jobs[1:]
	return j, nil
}

func (s *fakeStore) Heartbeat(_ context.Context, _ queue.Job, _ time.Duration, log queue.Log) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beats++
	s.flushed = append(s.flushed, log)
	if s.lostAt != 0 && s.beats >= s.lostAt {
		return queue.ErrLeaseLost
	}
	if s.cancelAt != 0 && s.beats >= s.cancelAt {
		return queue.ErrCancelled
	}
	return nil
}

func (s *fakeStore) Report(_ context.Context, _ queue.Job, r queue.Result) error {
	s.mu.Lock()
	s.reports = append(s.reports, r)
	s.mu.Unlock()
	s.reported <- struct{}{}
	return nil
}

func (s *fakeStore) waitReport(t *testing.T) queue.Result {
	t.Helper()
	select {
	case <-s.reported:
	case <-time.After(5 * time.Second):
		t.Fatal("no report within 5s")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reports[len(s.reports)-1]
}

// exitWith runs a real shell so the error is a genuine *exec.ExitError,
// which is what the sandbox returns and what result() has to read.
func exitWith(code int) error {
	return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
}

func serve(t *testing.T, w *Worker) (stop chan struct{}, cancel context.CancelFunc, done chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stop = make(chan struct{})
	done = make(chan struct{})
	go func() {
		defer close(done)
		w.Serve(ctx, stop, 1)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return stop, cancel, done
}

func TestAJobIsRunAndReported(t *testing.T) {
	store := newStore([]string{"/bin/sh", "-c", "exit 7"})
	var got sandbox.Spec
	w := &Worker{Store: store, Rootfs: "/img", Lease: time.Second, Poll: 10 * time.Millisecond,
		Run: func(_ context.Context, spec sandbox.Spec) error {
			got = spec
			spec.Stdout.Write([]byte("built\n"))
			spec.Stderr.Write([]byte("warn\n"))
			return exitWith(7)
		}}
	serve(t, w)

	r := store.waitReport(t)
	if got.Rootfs != "/img" || len(got.Args) != 3 || got.Args[2] != "exit 7" {
		t.Fatalf("the sandbox was given %+v", got)
	}
	if r.ExitCode != 7 || r.OOMKilled || r.Error != "" || string(r.Log.Tail) != "built\nwarn\n" || r.Log.Total != 11 {
		t.Fatalf("reported %+v", r)
	}
}

// TestTheWorkerFlushesTheLogWhileRunning: a heartbeat that goes out while
// the build is still writing carries what has been written so far.
func TestTheWorkerFlushesTheLogWhileRunning(t *testing.T) {
	store := newStore([]string{"slow"})
	w := &Worker{Store: store, Lease: 90 * time.Millisecond, Poll: 10 * time.Millisecond,
		Run: func(ctx context.Context, spec sandbox.Spec) error {
			spec.Stdout.Write([]byte("step 1\n"))
			time.Sleep(70 * time.Millisecond)
			spec.Stdout.Write([]byte("step 2\n"))
			return nil
		}}
	serve(t, w)
	store.waitReport(t)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.flushed) == 0 || string(store.flushed[0].Tail) != "step 1\n" || store.flushed[0].Total != 7 {
		t.Fatalf("heartbeats carried %+v, want the first to carry \"step 1\\n\"", store.flushed)
	}
}

func TestResultReadsWhatTheSandboxReturns(t *testing.T) {
	oom := fmt.Errorf("%w: %w", sandbox.ErrOOMKilled, exitWith(137))
	setup := exitWith(sandbox.SetupExitCode)
	// The shell itself killed by a signal: no status, ExitCode reports -1.
	signalled := exec.Command("sh", "-c", "kill -9 $$").Run()
	oomSignalled := fmt.Errorf("%w: %w", sandbox.ErrOOMKilled, exec.Command("sh", "-c", "kill -9 $$").Run())
	cases := map[string]struct {
		err  error
		want queue.Result
	}{
		"clean":   {nil, queue.Result{ExitCode: 0}},
		"status":  {exitWith(3), queue.Result{ExitCode: 3}},
		"oom":     {oom, queue.Result{ExitCode: 137, OOMKilled: true}},
		"signal":  {signalled, queue.Result{ExitCode: -1, Error: "killed: signal: killed"}},
		"oom-sig": {oomSignalled, queue.Result{ExitCode: -1, OOMKilled: true}},
		"setup":   {setup, queue.Result{ExitCode: 125, Error: "the sandbox could not be built; see the log"}},
		"noexec":  {errors.New("sandbox: requires Linux"), queue.Result{ExitCode: -1, Error: "sandbox: requires Linux"}},
		"context": {context.Canceled, queue.Result{ExitCode: -1, Error: "context canceled"}},
	}
	for name, c := range cases {
		if got := result(c.err); got.ExitCode != c.want.ExitCode || got.OOMKilled != c.want.OOMKilled || got.Error != c.want.Error {
			t.Errorf("%s: result(%v) = %+v, want %+v", name, c.err, got, c.want)
		}
	}
}

// TestALostLeaseKillsTheJob: the heartbeat comes back ErrLeaseLost, the
// build's context is cancelled, and a report is still attempted — the
// store is what refuses it, not the worker.
func TestALostLeaseKillsTheJob(t *testing.T) {
	store := newStore([]string{"sleep"})
	store.lostAt = 2
	cancelled := make(chan struct{})
	w := &Worker{Store: store, Lease: 60 * time.Millisecond, Poll: 10 * time.Millisecond,
		Run: func(ctx context.Context, _ sandbox.Spec) error {
			<-ctx.Done()
			close(cancelled)
			return ctx.Err()
		}}
	serve(t, w)

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the build was not cancelled after the lease was lost")
	}
	if r := store.waitReport(t); r.ExitCode != -1 || r.Error != "context canceled" {
		t.Fatalf("reported %+v after a lost lease", r)
	}
}

func TestDrainFinishesTheJobInHandAndClaimsNoMore(t *testing.T) {
	store := newStore([]string{"first"}, []string{"second"})
	started, release := make(chan struct{}), make(chan struct{})
	w := &Worker{Store: store, Lease: time.Second, Poll: 10 * time.Millisecond,
		Run: func(ctx context.Context, _ sandbox.Spec) error {
			started <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	stop, _, done := serve(t, w)

	<-started
	close(stop)
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after stop was closed and the job finished")
	}
	if r := store.waitReport(t); r.ExitCode != 0 || r.Error != "" {
		t.Fatalf("the job in hand was not allowed to finish: %+v", r)
	}
	if len(store.reports) != 1 || len(store.jobs) != 1 {
		t.Fatalf("%d reports, %d jobs left: the loop claimed after stop", len(store.reports), len(store.jobs))
	}
}

func TestCancelKillsTheJobInHand(t *testing.T) {
	store := newStore([]string{"hang"})
	started := make(chan struct{})
	w := &Worker{Store: store, Lease: time.Second, Poll: 10 * time.Millisecond,
		Run: func(ctx context.Context, _ sandbox.Spec) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}}
	_, cancel, done := serve(t, w)

	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after ctx was cancelled")
	}
}

func TestLogKeepsTheTail(t *testing.T) {
	l := &tail{limit: 8}
	l.Write([]byte("0123456789"))
	l.Write([]byte("ab"))
	if got := l.Log(); string(got.Tail) != "456789ab" || got.Total != 12 {
		t.Fatalf("tail = %+v, want the last 8 bytes of 12: %q", got, "456789ab")
	}
}

// TestMetricsCountTheWork: one job claimed and finished with a status, one
// killed by a lost lease, and the counters say so.
func TestMetricsCountTheWork(t *testing.T) {
	store := newStore([]string{"exit"}, []string{"lost"})
	store.lostAt = 2
	var reg metrics.Registry
	w := &Worker{Store: store, Lease: 60 * time.Millisecond, Poll: 10 * time.Millisecond, Metrics: &reg,
		Run: func(ctx context.Context, spec sandbox.Spec) error {
			if spec.Args[0] == "exit" {
				return exitWith(3)
			}
			<-ctx.Done()
			return ctx.Err()
		}}
	serve(t, w)
	store.waitReport(t)
	store.waitReport(t)

	var b strings.Builder
	reg.WriteTo(&b)
	got := b.String()
	for _, line := range []string{
		"forge_jobs_claimed_total 2\n",
		"forge_jobs_done_total{outcome=\"failed\"} 1\n",
		"forge_jobs_done_total{outcome=\"killed\"} 1\n",
		"forge_lease_lost_total 1\n",
		"forge_jobs_running 0\n",
	} {
		if !strings.Contains(got, line) {
			t.Errorf("metrics missing %q:\n%s", line, got)
		}
	}
}

// TestACancelledJobIsKilledAndReportedCancelled: the heartbeat says cancelled,
// the build is killed, and the report says why rather than "killed".
func TestACancelledJobIsKilledAndReportedCancelled(t *testing.T) {
	store := newStore([]string{"slow"})
	store.cancelAt = 2
	var reg metrics.Registry
	w := &Worker{Store: store, Lease: 60 * time.Millisecond, Poll: 10 * time.Millisecond, Metrics: &reg,
		Run: func(ctx context.Context, _ sandbox.Spec) error {
			// What the sandbox returns for a build whose context was cancelled:
			// PID 1 killed, so a signal and no exit status.
			<-ctx.Done()
			cmd := exec.Command("sleep", "10")
			cmd.Start()
			cmd.Process.Kill()
			return cmd.Wait()
		}}
	serve(t, w)

	r := store.waitReport(t)
	if r.ExitCode != -1 || r.Error != queue.ReasonCancelled {
		t.Fatalf("reported %+v, want exit -1 and error %q", r, queue.ReasonCancelled)
	}
	var b strings.Builder
	reg.WriteTo(&b)
	for _, line := range []string{
		"forge_jobs_done_total{outcome=\"cancelled\"} 1\n",
		"forge_jobs_done_total{outcome=\"killed\"} 0\n",
		"forge_lease_lost_total 0\n",
	} {
		if !strings.Contains(b.String(), line) {
			t.Errorf("metrics missing %q:\n%s", line, b.String())
		}
	}
}

// TestABuildThatFinishesAsItIsCancelledKeepsItsResult: a cancel noticed after
// the build already exited on its own must not overwrite what it did.
func TestABuildThatFinishesAsItIsCancelledKeepsItsResult(t *testing.T) {
	store := newStore([]string{"done"})
	store.cancelAt = 1
	w := &Worker{Store: store, Lease: 60 * time.Millisecond, Poll: 10 * time.Millisecond,
		Run: func(ctx context.Context, _ sandbox.Spec) error {
			<-ctx.Done()
			return exitWith(5)
		}}
	serve(t, w)

	if r := store.waitReport(t); r.ExitCode != 5 || r.Error != "" {
		t.Fatalf("reported %+v, want the build's own exit 5", r)
	}
}

// TestAJobClaimedAlreadyCancelledIsNotRun: its earlier worker died before
// noticing the cancel; this one reports it without starting the build.
func TestAJobClaimedAlreadyCancelledIsNotRun(t *testing.T) {
	store := newStore([]string{"never"})
	store.jobs[0].Cancelled = true
	ran := false
	w := &Worker{Store: store, Lease: time.Second, Poll: 10 * time.Millisecond,
		Run: func(context.Context, sandbox.Spec) error {
			ran = true
			return nil
		}}
	serve(t, w)

	if r := store.waitReport(t); r.ExitCode != -1 || r.Error != queue.ReasonCancelled || ran {
		t.Fatalf("reported %+v (ran: %v), want cancelled without running", r, ran)
	}
}
