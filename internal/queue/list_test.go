package queue

import (
	"context"
	"testing"
	"time"
)

func ids(js []Summary) []int64 {
	out := make([]int64, len(js))
	for i, j := range js {
		out[i] = j.ID
	}
	return out
}

func TestListIsNewestFirstAndPagesByID(t *testing.T) {
	q, ctx := open(t), context.Background()
	for i := 0; i < 5; i++ {
		if _, err := q.Submit(ctx, []string{"echo", string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := q.List(ctx, "", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page); len(got) != 2 || got[0] != 5 || got[1] != 4 {
		t.Fatalf("first page = %v, want [5 4]", got)
	}
	if page[0].Args[1] != "e" || page[0].State != "queued" || page[0].Result.ExitCode != -1 {
		t.Fatalf("newest job reads %+v", page[0])
	}
	// A job submitted between pages must not shift the next one.
	q.Submit(ctx, []string{"late"})
	page, _ = q.List(ctx, "", page[1].ID, 2)
	if got := ids(page); len(got) != 2 || got[0] != 3 || got[1] != 2 {
		t.Fatalf("second page = %v, want [3 2]", got)
	}
	page, _ = q.List(ctx, "", 2, 2)
	if got := ids(page); len(got) != 1 || got[0] != 1 {
		t.Fatalf("last page = %v, want [1]", got)
	}
}

func TestListFiltersByState(t *testing.T) {
	q, ctx := open(t), context.Background()
	q.Submit(ctx, []string{"one"})
	q.Submit(ctx, []string{"two"})
	q.Submit(ctx, []string{"three"})
	job, _ := q.Claim(ctx, "w1", time.Minute) // job 1 running
	q.Report(ctx, job, Result{ExitCode: 2, Log: Log{Total: 9}})
	q.Claim(ctx, "w1", time.Minute) // job 2 running

	for state, want := range map[string][]int64{"queued": {3}, "running": {2}, "done": {1}} {
		page, err := q.List(ctx, state, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(page); len(got) != len(want) || got[0] != want[0] {
			t.Errorf("state %s: %v, want %v", state, got, want)
		}
	}
	done, _ := q.List(ctx, "done", 0, 10)
	if d := done[0]; d.Result.ExitCode != 2 || d.Worker != "w1" || d.Result.Log.Total != 9 || d.Result.Log.Tail != nil {
		t.Fatalf("a listed done job reads %+v: exit, worker, byte count and no log", d)
	}
}
