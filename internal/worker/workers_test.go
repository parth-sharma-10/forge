package worker

// Phase 4: more than one worker against the real queue. These need a
// PostgreSQL and nothing else — the runner is fake, driven by a job's args,
// because what is under test is what happens between workers, not inside
// the sandbox. The one thing a goroutine cannot do is die, so the worker
// that dies is a real process: the test binary in its fourth identity.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/parth-sharma-10/forge/internal/queue"
	"github.com/parth-sharma-10/forge/internal/sandbox"
)

const (
	dsnEnv    = "FORGE_TEST_DSN"
	workerArg = "forge-test-worker"
)

func TestMain(m *testing.M) {
	sandbox.MaybeChild()
	if len(os.Args) > 2 && os.Args[1] == workerArg {
		runTestWorker(os.Args[2])
	}
	os.Exit(m.Run())
}

// fakeRun is the runner every worker in this file uses. A job's args are
// its script: "sleep 30s" waits, "exit 3" exits 3, "ok" exits 0.
func fakeRun(ctx context.Context, spec sandbox.Spec) error {
	switch spec.Args[0] {
	case "sleep":
		d, _ := time.ParseDuration(spec.Args[1])
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case "exit":
		code, _ := strconv.Atoi(spec.Args[1])
		return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	}
	return nil
}

// runTestWorker is the fourth identity: a worker process with the fake
// runner, serving until it is killed.
func runTestWorker(name string) {
	q, err := queue.Open(context.Background(), os.Getenv(dsnEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	w := &Worker{Store: q, Name: name, Lease: time.Second, Poll: 50 * time.Millisecond, Run: fakeRun}
	w.Serve(context.Background(), make(chan struct{}), 1)
	os.Exit(0)
}

// openQueue connects to the test database and empties the table, with the
// same skip-or-fail rule as the queue's own tests.
func openQueue(t testing.TB) *queue.Queue {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set: the multi-worker tests must run in CI", dsnEnv)
		}
		t.Skipf("%s not set", dsnEnv)
	}
	ctx := context.Background()
	q, err := queue.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `TRUNCATE jobs RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
	return q
}

// waitFor polls the job until cond holds, or fails after 15s.
func waitFor(t *testing.T, q *queue.Queue, id int64, what string, cond func(queue.Status) bool) queue.Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s, err := q.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if cond(s) {
			return s
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("job %d: %s did not happen within 15s", id, what)
	return queue.Status{}
}

func startWorker(t *testing.T, w *Worker) (stop chan struct{}, done chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stop, done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		w.Serve(ctx, stop, 1)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return stop, done
}

// TestAJobSurvivesItsWorker: a worker process is SIGKILLed while running a
// job. Its lease expires, a second worker reclaims the job, runs it again
// and reports it. The row says it took two attempts and who ran the last.
func TestAJobSurvivesItsWorker(t *testing.T) {
	q, ctx := openQueue(t), context.Background()
	id, err := q.Submit(ctx, []string{"sleep", "30s"})
	if err != nil {
		t.Fatal(err)
	}

	victim := exec.Command(os.Args[0], workerArg, "victim")
	victim.Env = append(os.Environ(), dsnEnv+"="+os.Getenv(dsnEnv))
	victim.Stderr = os.Stderr
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { victim.Process.Kill(); victim.Wait() })

	waitFor(t, q, id, "claim by the victim", func(s queue.Status) bool { return s.State == "running" && s.Worker == "victim" })
	if err := victim.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	victim.Wait()

	rescuer := &Worker{Store: q, Name: "rescuer", Lease: time.Second, Poll: 50 * time.Millisecond,
		Run: func(ctx context.Context, spec sandbox.Spec) error { return nil }}
	startWorker(t, rescuer)

	s := waitFor(t, q, id, "the rescuer finishing it", func(s queue.Status) bool { return s.State == "done" })
	if s.Worker != "rescuer" || s.Attempt != 2 || s.Result.ExitCode != 0 || s.Result.Error != "" {
		t.Fatalf("after the victim died: %+v, want done by the rescuer on attempt 2", s)
	}
}

// silentStore stops heartbeating without stopping anything else: the
// worker believes it still holds the job, the queue knows otherwise.
type silentStore struct{ *queue.Queue }

func (silentStore) Heartbeat(context.Context, queue.Job, time.Duration, queue.Log) error { return nil }

// TestALateWorkersReportIsRefused: worker A stops heartbeating, loses the
// lease, and finishes its build while B — which reclaimed the job — is
// still running it. A's report is refused: the row stays B's, still
// running, and B's result is the one recorded. The state check alone would
// not do this (B's row is running, which is what A's report matches on);
// the fencing token is what does.
func TestALateWorkersReportIsRefused(t *testing.T) {
	q, ctx := openQueue(t), context.Background()
	id, _ := q.Submit(ctx, []string{"exit", "7"})

	releaseA, releaseB := make(chan struct{}), make(chan struct{})
	// Both runners honour ctx, or a failed assertion leaves Serve waiting
	// on a build that never ends and the test hangs instead of failing.
	a := &Worker{Store: silentStore{q}, Name: "a", Lease: 300 * time.Millisecond, Poll: 50 * time.Millisecond,
		Run: func(ctx context.Context, spec sandbox.Spec) error {
			select {
			case <-releaseA:
				return fakeRun(ctx, spec) // exit 7, A's answer
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	startWorker(t, a)
	waitFor(t, q, id, "claim by a", func(s queue.Status) bool { return s.Worker == "a" })

	b := &Worker{Store: q, Name: "b", Lease: 5 * time.Second, Poll: 50 * time.Millisecond,
		Run: func(ctx context.Context, spec sandbox.Spec) error {
			select {
			case <-releaseB:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	startWorker(t, b)
	waitFor(t, q, id, "reclaim by b", func(s queue.Status) bool { return s.Worker == "b" && s.Attempt == 2 })

	close(releaseA)
	time.Sleep(300 * time.Millisecond) // long enough for a's report to have been tried
	if s, _ := q.Get(ctx, id); s.State != "running" || s.Worker != "b" {
		t.Fatalf("a's late report changed the row while b was running: %+v", s)
	}

	close(releaseB)
	s := waitFor(t, q, id, "b finishing", func(s queue.Status) bool { return s.State == "done" })
	if s.Worker != "b" || s.Attempt != 2 || s.Result.ExitCode != 0 {
		t.Fatalf("final row %+v, want b's exit 0 on attempt 2", s)
	}
}

// TestALiveLeaseIsExclusive: while one worker holds a job, any number of
// claimers hammering the queue get nothing.
func TestALiveLeaseIsExclusive(t *testing.T) {
	q, ctx := openQueue(t), context.Background()
	id, _ := q.Submit(ctx, []string{"sleep", "30s"})
	holder := &Worker{Store: q, Name: "holder", Lease: 5 * time.Second, Poll: 50 * time.Millisecond, Run: fakeRun}
	startWorker(t, holder)
	waitFor(t, q, id, "claim by the holder", func(s queue.Status) bool { return s.Worker == "holder" })

	var wg sync.WaitGroup
	var claimed int
	var mu sync.Mutex
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := q.Claim(ctx, "thief", time.Second); !errors.Is(err, queue.ErrEmpty) {
					mu.Lock()
					claimed++
					mu.Unlock()
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	if s, _ := q.Get(ctx, id); claimed != 0 || s.Attempt != 1 || s.Worker != "holder" {
		t.Fatalf("%d claims succeeded against a live lease; row %+v", claimed, s)
	}
}

// TestAJobIsGivenUpAfterMaxAttempts: two attempts already spent on the row
// — leases claimed and left to expire — and a worker with MaxAttempts 2
// handed the third reports it as given up without running it.
func TestAJobIsGivenUpAfterMaxAttempts(t *testing.T) {
	q, ctx := openQueue(t), context.Background()
	id, _ := q.Submit(ctx, []string{"exit", "0"})
	for i := 0; i < 2; i++ {
		if _, err := q.Claim(ctx, "dead", 20*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
	}

	var ran atomic.Bool
	w := &Worker{Store: q, Name: "w", MaxAttempts: 2, Lease: time.Second, Poll: 20 * time.Millisecond,
		Run: func(ctx context.Context, spec sandbox.Spec) error { ran.Store(true); return nil }}
	startWorker(t, w)
	s := waitFor(t, q, id, "the third attempt", func(s queue.Status) bool { return s.State == "done" })
	if ran.Load() || s.Attempt != 3 || !strings.HasPrefix(s.Result.Error, "given up: attempt 3, limit 2") || s.Result.ExitCode != -1 {
		t.Fatalf("ran=%v, row %+v: want given up on attempt 3 without running", ran.Load(), s)
	}
}
