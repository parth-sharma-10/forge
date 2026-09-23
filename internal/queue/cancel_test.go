package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCancellingAQueuedJobFinishesIt: no worker ever sees it.
func TestCancellingAQueuedJobFinishesIt(t *testing.T) {
	q, ctx := open(t), context.Background()
	id, _ := q.Submit(ctx, []string{"true"})

	state, err := q.Cancel(ctx, id)
	if err != nil || state != "done" {
		t.Fatalf("cancel of a queued job = %q, %v; want done", state, err)
	}
	s, _ := q.Get(ctx, id)
	if s.State != "done" || !s.Cancelled || s.Result.ExitCode != -1 || s.Result.Error != "cancelled" {
		t.Fatalf("after cancel: %+v", s)
	}
	if _, err := q.Claim(ctx, "w1", time.Minute); !errors.Is(err, ErrEmpty) {
		t.Fatalf("claim after the only job was cancelled: %v, want ErrEmpty", err)
	}
}

// TestCancellingARunningJobReachesItsWorker: the job stays running, the
// worker's next heartbeat says cancelled, and its report still lands.
func TestCancellingARunningJobReachesItsWorker(t *testing.T) {
	q, ctx := open(t), context.Background()
	id, _ := q.Submit(ctx, []string{"sleep", "1000"})
	job, _ := q.Claim(ctx, "w1", time.Minute)

	if err := q.Heartbeat(ctx, job, time.Minute, Log{}); err != nil {
		t.Fatalf("heartbeat before cancel: %v", err)
	}
	state, err := q.Cancel(ctx, id)
	if err != nil || state != "running" {
		t.Fatalf("cancel of a running job = %q, %v; want running", state, err)
	}
	if err := q.Heartbeat(ctx, job, time.Minute, Log{}); !errors.Is(err, ErrCancelled) {
		t.Fatalf("heartbeat after cancel: %v, want ErrCancelled", err)
	}
	if err := q.Report(ctx, job, Result{ExitCode: -1, Error: "cancelled"}); err != nil {
		t.Fatalf("the cancelled worker's report was refused: %v", err)
	}
	if s, _ := q.Get(ctx, id); s.State != "done" || !s.Cancelled || s.Result.Error != "cancelled" {
		t.Fatalf("after the report: %+v", s)
	}
}

// TestACancelledJobWhoseWorkerDiedIsNotRunAgain: the lease expires before the
// worker noticed; whoever reclaims it is told it is cancelled.
func TestACancelledJobWhoseWorkerDiedIsNotRunAgain(t *testing.T) {
	q, ctx := open(t), context.Background()
	id, _ := q.Submit(ctx, []string{"true"})
	if _, err := q.Claim(ctx, "w1", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	job, err := q.Claim(ctx, "w2", time.Minute)
	if err != nil || job.ID != id || !job.Cancelled {
		t.Fatalf("reclaim = %+v, %v; want job %d marked cancelled", job, err, id)
	}
}

// TestCancelLeavesAFinishedResultAlone and says why it refused.
func TestCancelLeavesAFinishedResultAlone(t *testing.T) {
	q, ctx := open(t), context.Background()
	id, _ := q.Submit(ctx, []string{"true"})
	job, _ := q.Claim(ctx, "w1", time.Minute)
	if err := q.Report(ctx, job, Result{ExitCode: 0}); err != nil {
		t.Fatal(err)
	}

	if _, err := q.Cancel(ctx, id); !errors.Is(err, ErrDone) {
		t.Fatalf("cancel of a done job: %v, want ErrDone", err)
	}
	if s, _ := q.Get(ctx, id); s.Cancelled || s.Result.ExitCode != 0 || s.Result.Error != "" {
		t.Fatalf("a finished job was changed by cancel: %+v", s)
	}
	if _, err := q.Cancel(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel of an unknown id: %v, want ErrNotFound", err)
	}
}
