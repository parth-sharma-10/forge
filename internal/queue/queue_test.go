package queue

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

const dsnEnv = "FORGE_TEST_DSN"

// open connects to the test database and empties the table. A real
// PostgreSQL, never a mock: the claim query's whole substance is what the
// database does under FOR UPDATE SKIP LOCKED, and a mock would be a test of
// the mock. Skipped on a machine without one; a failure under CI, where a
// skip would let the gate pass with the queue untested.
func open(t *testing.T) *Queue {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set: the queue tests must run in CI", dsnEnv)
		}
		t.Skipf("%s not set: e.g. postgres://forge@127.0.0.1:5432/postgres", dsnEnv)
	}
	q, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	if _, err := q.db.Exec(context.Background(), `TRUNCATE jobs RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestSubmitClaimReport(t *testing.T) {
	q, ctx := open(t), context.Background()
	id, err := q.Submit(ctx, []string{"/bin/sh", "-c", "exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := q.Get(ctx, id); s.State != "queued" || s.Attempt != 0 {
		t.Fatalf("after submit: %+v, want queued, attempt 0", s)
	}

	job, err := q.Claim(ctx, "w1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != id || len(job.Args) != 3 || job.Args[2] != "exit 3" || job.Fence != 1 || job.Attempt != 1 {
		t.Fatalf("claimed %+v, want job %d with its args, fence 1, attempt 1", job, id)
	}
	if until := time.Until(job.LeaseUntil); until < 50*time.Second || until > time.Minute {
		t.Fatalf("lease runs out in %v, want about a minute", until)
	}
	if s, _ := q.Get(ctx, id); s.State != "running" || s.Attempt != 1 || s.Worker != "w1" {
		t.Fatalf("after claim: %+v, want running by w1, attempt 1", s)
	}

	if err := q.Report(ctx, job, Result{ExitCode: 3, Log: Log{Tail: []byte("built\n"), Total: 6}}); err != nil {
		t.Fatal(err)
	}
	s, err := q.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "done" || s.Result.ExitCode != 3 || string(s.Result.Log.Tail) != "built\n" || s.Result.Log.Total != 6 || s.Result.OOMKilled {
		t.Fatalf("after report: %+v", s)
	}
	if _, err := q.Claim(ctx, "w1", time.Minute); !errors.Is(err, ErrEmpty) {
		t.Fatalf("claim after the only job is done: %v, want ErrEmpty", err)
	}
}

func TestEmptyQueueAndUnknownJob(t *testing.T) {
	q, ctx := open(t), context.Background()
	if _, err := q.Claim(ctx, "w1", time.Minute); !errors.Is(err, ErrEmpty) {
		t.Fatalf("claim on an empty queue: %v, want ErrEmpty", err)
	}
	if _, err := q.Get(ctx, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get of an unsubmitted id: %v, want ErrNotFound", err)
	}
	if _, err := q.Submit(ctx, nil); err == nil {
		t.Fatal("submit with no command succeeded")
	}
}

// TestClaimIsExclusive is the SKIP LOCKED assertion: many claimers at once,
// fewer jobs than claimers, and every job goes to exactly one of them.
func TestClaimIsExclusive(t *testing.T) {
	q, ctx := open(t), context.Background()
	const jobs, claimers = 5, 20
	for i := 0; i < jobs; i++ {
		if _, err := q.Submit(ctx, []string{"true"}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	got := map[int64]int{}
	var empty int
	var wg sync.WaitGroup
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := q.Claim(ctx, "w", time.Minute)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrEmpty):
				empty++
			case err != nil:
				t.Error(err)
			default:
				got[job.ID]++
			}
		}()
	}
	wg.Wait()

	if len(got) != jobs || empty != claimers-jobs {
		t.Fatalf("%d claimers over %d jobs: %d distinct jobs claimed, %d came back empty", claimers, jobs, len(got), empty)
	}
	for id, n := range got {
		if n != 1 {
			t.Errorf("job %d was claimed %d times", id, n)
		}
	}
}

// TestAnExpiredLeaseIsReclaimed is what makes the lease a lease. The second
// claim gets the same job, one attempt and one fence later.
func TestAnExpiredLeaseIsReclaimed(t *testing.T) {
	q, ctx := open(t), context.Background()
	id, _ := q.Submit(ctx, []string{"true"})
	first, err := q.Claim(ctx, "w1", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(ctx, "w2", time.Minute); !errors.Is(err, ErrEmpty) {
		t.Fatalf("claim while the lease is live: %v, want ErrEmpty", err)
	}

	time.Sleep(100 * time.Millisecond)
	second, err := q.Claim(ctx, "w2", time.Minute)
	if err != nil {
		t.Fatalf("claim after the lease expired: %v", err)
	}
	if second.ID != id || second.Fence != first.Fence+1 || second.Attempt != 2 {
		t.Fatalf("reclaimed %+v, want job %d with fence %d, attempt 2", second, id, first.Fence+1)
	}
	if s, _ := q.Get(ctx, id); s.Attempt != 2 || s.Worker != "w2" {
		t.Fatalf("after reclaim: %+v, want attempt 2 by w2", s)
	}
}

// TestAStaleFenceCannotReportOrHeartbeat is the fencing token doing its one
// job: the worker that lost the lease still has a Job in hand, and nothing
// it does with it reaches the row.
func TestAStaleFenceCannotReportOrHeartbeat(t *testing.T) {
	q, ctx := open(t), context.Background()
	id, _ := q.Submit(ctx, []string{"true"})
	stale, _ := q.Claim(ctx, "w1", 50*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	live, err := q.Claim(ctx, "w2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if err := q.Heartbeat(ctx, stale, time.Minute, Log{}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("heartbeat with a stale fence: %v, want ErrLeaseLost", err)
	}
	if err := q.Report(ctx, stale, Result{ExitCode: 0}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("report with a stale fence: %v, want ErrLeaseLost", err)
	}
	if s, _ := q.Get(ctx, id); s.State != "running" || s.Worker != "w2" {
		t.Fatalf("the stale report changed the row: %+v", s)
	}

	if err := q.Heartbeat(ctx, live, time.Minute, Log{}); err != nil {
		t.Fatalf("heartbeat with the live fence: %v", err)
	}
	if err := q.Report(ctx, live, Result{ExitCode: 0}); err != nil {
		t.Fatalf("report with the live fence: %v", err)
	}
	if err := q.Report(ctx, live, Result{ExitCode: 1}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a second report of a done job: %v, want ErrLeaseLost", err)
	}
}

func TestHeartbeatExtendsTheLease(t *testing.T) {
	q, ctx := open(t), context.Background()
	q.Submit(ctx, []string{"true"})
	job, _ := q.Claim(ctx, "w1", 100*time.Millisecond)
	for i := 0; i < 3; i++ {
		time.Sleep(60 * time.Millisecond)
		if err := q.Heartbeat(ctx, job, 100*time.Millisecond, Log{}); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	// 180ms in, well past the original 100ms lease.
	if _, err := q.Claim(ctx, "w2", time.Minute); !errors.Is(err, ErrEmpty) {
		t.Fatalf("claim while the heartbeats kept the lease: %v, want ErrEmpty", err)
	}
}

// TestHeartbeatCarriesTheLog: what the worker has so far is readable while
// the job runs, and the count beside it says how much of the beginning is
// missing from the tail.
func TestHeartbeatCarriesTheLog(t *testing.T) {
	q, ctx := open(t), context.Background()
	id, _ := q.Submit(ctx, []string{"true"})
	job, _ := q.Claim(ctx, "w1", time.Minute)
	if err := q.Heartbeat(ctx, job, time.Minute, Log{Tail: []byte("cdef"), Total: 6}); err != nil {
		t.Fatal(err)
	}
	s, err := q.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "running" || string(s.Result.Log.Tail) != "cdef" || s.Result.Log.Total != 6 {
		t.Fatalf("while running: %+v, want the flushed tail and total", s)
	}
}

// TestConcurrentOpensOnAFreshDatabase is the schema race: the API and the
// worker start together on every deployment, and CREATE TABLE IF NOT EXISTS
// is not safe against itself. Without the advisory lock one of these fails
// on the catalogue's unique index.
func TestConcurrentOpensOnAFreshDatabase(t *testing.T) {
	q, ctx := open(t), context.Background()
	if _, err := q.db.Exec(ctx, `DROP TABLE jobs`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q, err := Open(ctx, os.Getenv(dsnEnv))
			if err != nil {
				errs <- err
				return
			}
			q.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Open: %v", err)
	}
}
