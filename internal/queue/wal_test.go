package queue

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestAnUnchangedLogIsNotRewritten: a heartbeat whose log has not grown since
// the last one must not write the log again. A quiet build holding a full
// 1 MiB tail heartbeats every two seconds for as long as it runs; rewriting
// the tail each time is half a megabyte of WAL a second for no new output.
func TestAnUnchangedLogIsNotRewritten(t *testing.T) {
	q, ctx := open(t), context.Background()
	q.Submit(ctx, []string{"quiet"})
	job, _ := q.Claim(ctx, "w1", time.Minute)
	// Incompressible, so TOAST cannot shrink it and hide the rewrite.
	tail := make([]byte, 1<<20)
	for i := range tail {
		tail[i] = byte(i*7919 ^ i>>3)
	}
	log := Log{Tail: tail, Total: int64(len(tail))}
	if err := q.Heartbeat(ctx, job, time.Minute, log); err != nil {
		t.Fatal(err)
	}

	wal := func() int64 {
		var lsn int64
		if err := q.db.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn() - '0/0'::pg_lsn`).Scan(&lsn); err != nil {
			t.Fatal(err)
		}
		return lsn
	}
	const beats = 10
	before := wal()
	for i := 0; i < beats; i++ {
		if err := q.Heartbeat(ctx, job, time.Minute, log); err != nil {
			t.Fatal(err)
		}
	}
	perBeat := (wal() - before) / beats
	t.Logf("WAL per heartbeat with an unchanged 1 MiB log: %d bytes", perBeat)
	if perBeat > 64<<10 {
		t.Fatalf("each heartbeat wrote %d bytes of WAL for a log that had not changed", perBeat)
	}

	// And a log that did grow is still stored.
	log.Tail = append(log.Tail[1:], 'x')
	log.Total++
	if err := q.Heartbeat(ctx, job, time.Minute, log); err != nil {
		t.Fatal(err)
	}
	s, _ := q.Get(ctx, job.ID)
	if s.Result.Log.Total != log.Total || !bytes.Equal(s.Result.Log.Tail, log.Tail) {
		t.Fatalf("a grown log was not stored: total %d, want %d", s.Result.Log.Total, log.Total)
	}
}

// TestAReclaimedJobDoesNotKeepTheDeadAttemptsLog: a new attempt whose output
// happens to be as long as the dead one's must store its own, not keep the
// old bytes because the count matched.
func TestAReclaimedJobDoesNotKeepTheDeadAttemptsLog(t *testing.T) {
	q, ctx := open(t), context.Background()
	q.Submit(ctx, []string{"flaky"})
	first, _ := q.Claim(ctx, "w1", 50*time.Millisecond)
	q.Heartbeat(ctx, first, 50*time.Millisecond, Log{Tail: []byte("hello"), Total: 5})
	time.Sleep(100 * time.Millisecond)

	second, err := q.Claim(ctx, "w2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := q.Get(ctx, second.ID); s.Result.Log.Total != 0 || len(s.Result.Log.Tail) != 0 {
		t.Fatalf("a new attempt starts with the old log: %+v", s.Result.Log)
	}
	if err := q.Heartbeat(ctx, second, time.Minute, Log{Tail: []byte("world"), Total: 5}); err != nil {
		t.Fatal(err)
	}
	if s, _ := q.Get(ctx, second.ID); string(s.Result.Log.Tail) != "world" {
		t.Fatalf("log = %q, want the second attempt's %q", s.Result.Log.Tail, "world")
	}
}
