package queue

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPruneRemovesOnlyOldFinishedJobs: a job that finished a day ago goes; one
// that finished just now, one running and one queued all stay.
func TestPruneRemovesOnlyOldFinishedJobs(t *testing.T) {
	q, ctx := open(t), context.Background()
	for i := 0; i < 4; i++ {
		q.Submit(ctx, []string{"true"})
	}
	for i := 0; i < 2; i++ {
		job, _ := q.Claim(ctx, "w1", time.Minute)
		q.Report(ctx, job, Result{})
	}
	q.Claim(ctx, "w1", time.Minute) // job 3 running, job 4 queued
	if _, err := q.db.Exec(ctx, `UPDATE jobs SET finished_at = now() - interval '1 day' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	n, err := q.Prune(ctx, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v; want 1", n, err)
	}
	if _, err := q.Get(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job 1 finished a day ago and survived a 1h prune: %v", err)
	}
	for _, id := range []int64{2, 3, 4} {
		if _, err := q.Get(ctx, id); err != nil {
			t.Errorf("job %d was pruned: %v", id, err)
		}
	}
}
