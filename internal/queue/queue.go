// Package queue is Forge's job queue: one PostgreSQL table, claimed with
// FOR UPDATE SKIP LOCKED. Durable state and the queue are the same rows, so
// there is nothing to keep consistent between a database and a broker.
//
// A claim hands out a lease and a fencing token. The lease is a deadline the
// worker extends by heartbeating; a job whose lease has passed is claimable
// again, by anyone. The token is a counter that increments on every claim,
// and every write a worker makes afterwards — heartbeat, report — names the
// token it was given. A worker that lost its lease still holds the old
// token, so its writes match no row, and what it does with its stale copy
// of the job cannot reach the queue.
package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrEmpty is returned by Claim when nothing is queued and no lease has
// expired.
var ErrEmpty = errors.New("queue: nothing to claim")

// ErrLeaseLost is returned by Heartbeat and Report when the job is no
// longer this worker's: the lease expired and another claim took it, so
// the fencing token no longer matches. The right response is to stop the
// build, not to retry.
var ErrLeaseLost = errors.New("queue: lease lost")

// ErrNotFound is returned by Get and Cancel for an id that was never
// submitted.
var ErrNotFound = errors.New("queue: no such job")

// ErrCancelled is returned by Heartbeat when the job has been cancelled: the
// worker should kill the build and report it. Unlike ErrLeaseLost the job is
// still this worker's, and its report is accepted.
var ErrCancelled = errors.New("queue: job cancelled")

// ErrDone is returned by Cancel for a job that has already finished.
var ErrDone = errors.New("queue: job already done")

// ReasonCancelled is the error a cancelled job is finished with, by Cancel
// for a queued job or by the worker for a running one.
const ReasonCancelled = "cancelled"

// schema is applied by Open. CREATE IF NOT EXISTS rather than a migration
// tool: there is one table, and the ALTERs at the end are the columns added
// after it first existed (log_bytes in Phase 3, cancel_requested in v2),
// each idempotent.
//
// ponytail: no attempt cap. A job that kills its worker every time is
// reclaimed forever; Phase 4 owns the ceiling, with the runs history it
// will want to decide it.
const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id          bigserial PRIMARY KEY,
	args        text[]      NOT NULL,
	state       text        NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done')),
	attempt     int         NOT NULL DEFAULT 0,
	fence       bigint      NOT NULL DEFAULT 0,
	worker      text        NOT NULL DEFAULT '',
	lease_until timestamptz,
	exit_code   int,
	oom_killed  boolean     NOT NULL DEFAULT false,
	error       text        NOT NULL DEFAULT '',
	log         bytea       NOT NULL DEFAULT '',
	log_bytes   bigint      NOT NULL DEFAULT 0,
	created_at  timestamptz NOT NULL DEFAULT now(),
	started_at  timestamptz,
	finished_at timestamptz
);
CREATE INDEX IF NOT EXISTS jobs_claimable ON jobs (id) WHERE state <> 'done';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS log_bytes bigint NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS cancel_requested boolean NOT NULL DEFAULT false;
`

// Queue is a handle on the jobs table. It is safe for concurrent use.
type Queue struct {
	db *pgxpool.Pool
}

// schemaLock is the advisory lock key the schema is applied under. Two
// processes running CREATE TABLE IF NOT EXISTS at the same moment on a
// fresh database do not both succeed — one of them hits the catalogue's
// unique index — and the API and the worker start at the same moment on
// every deployment. Measured, not supposed: the first run of both against
// an empty database killed the worker with exactly that error.
const schemaLock = 0x666f726765 // "forge"

// Open connects to PostgreSQL and makes sure the table exists.
func Open(ctx context.Context, dsn string) (*Queue, error) {
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	// applySchema has released its connection by the time it returns;
	// Close waits for every connection to come back, so it must not be
	// called with one still held inside a deferred rollback.
	if err := applySchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Queue{db: db}, nil
}

func applySchema(ctx context.Context, db *pgxpool.Pool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(schemaLock)); err != nil {
		return fmt.Errorf("queue: schema lock: %w", err)
	}
	if _, err := tx.Exec(ctx, schema); err != nil {
		return fmt.Errorf("queue: apply schema: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("queue: apply schema: %w", err)
	}
	return nil
}

// Close releases the connections. Leases held by this process are not
// released: they expire, and are reclaimed, on their own.
func (q *Queue) Close() { q.db.Close() }

// Submit queues a command. It returns the job's id.
func (q *Queue) Submit(ctx context.Context, args []string) (int64, error) {
	if len(args) == 0 {
		return 0, errors.New("queue: submit: no command")
	}
	var id int64
	err := q.db.QueryRow(ctx, `INSERT INTO jobs (args) VALUES ($1) RETURNING id`, args).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("queue: submit: %w", err)
	}
	return id, nil
}

// Job is a claimed job: what to run, the token that proves the claim, and
// which attempt this is — the first, or one more after a worker died.
type Job struct {
	ID         int64
	Args       []string
	Fence      int64
	Attempt    int
	LeaseUntil time.Time
	// Cancelled means the job was cancelled while an earlier attempt ran and
	// that worker died before reporting it. The claimer reports it cancelled
	// rather than running it.
	Cancelled bool
}

// Claim takes the oldest job that is queued or whose lease has expired,
// marks it running under worker for lease, and returns it. Two workers
// claiming at once get two different jobs, or one gets ErrEmpty: SKIP
// LOCKED means a row another transaction is claiming is passed over, not
// waited for.
//
// An expired lease is claimed here, in the same statement, rather than by a
// sweeper that requeues it first. There is no window in which the job is
// queued but not claimable, and no sweeper to fail to run.
func (q *Queue) Claim(ctx context.Context, worker string, lease time.Duration) (Job, error) {
	var j Job
	err := q.db.QueryRow(ctx, `
		UPDATE jobs
		SET state = 'running', attempt = attempt + 1, fence = fence + 1,
		    worker = $1, lease_until = now() + $2, started_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE state = 'queued' OR (state = 'running' AND lease_until < now())
			ORDER BY id
			FOR UPDATE SKIP LOCKED
			LIMIT 1)
		RETURNING id, args, fence, attempt, lease_until, cancel_requested`,
		worker, lease).Scan(&j.ID, &j.Args, &j.Fence, &j.Attempt, &j.LeaseUntil, &j.Cancelled)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrEmpty
	}
	if err != nil {
		return Job{}, fmt.Errorf("queue: claim: %w", err)
	}
	return j, nil
}

// Log is the build's output as the worker holds it: the last bytes, and
// how many there have been in all, so a reader can tell how much of the
// beginning is gone.
type Log struct {
	Tail  []byte
	Total int64
}

// Heartbeat extends the lease by lease from now and stores the log so far.
// ErrLeaseLost means the job was reclaimed: the worker should kill the
// build. ErrCancelled means someone cancelled it: the worker should kill the
// build and report it, which the lease it still holds lets it do.
//
// Cancellation rides the heartbeat rather than a channel of its own, so it
// reaches a running build within one heartbeat interval and needs nothing
// listening on the worker.
func (q *Queue) Heartbeat(ctx context.Context, job Job, lease time.Duration, log Log) error {
	var cancelled bool
	err := q.db.QueryRow(ctx, `
		UPDATE jobs SET lease_until = now() + $3, log = COALESCE($4, ''::bytea), log_bytes = $5
		WHERE id = $1 AND fence = $2 AND state = 'running'
		RETURNING cancel_requested`,
		job.ID, job.Fence, lease, log.Tail, log.Total).Scan(&cancelled)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("queue: heartbeat: %w", err)
	}
	if cancelled {
		return ErrCancelled
	}
	return nil
}

// Cancel cancels a job and returns the state it is left in. A queued job is
// finished on the spot — no worker ever sees it — and comes back "done". A
// running one is marked, and comes back "running": its worker learns at its
// next heartbeat, kills the build and reports it, so it is done within one
// heartbeat interval of the call. ErrDone for a job that already finished,
// whose result stands; ErrNotFound for one that never existed.
//
// One statement, so a claim racing it either took the row first — the cancel
// then waits on the row lock and finds it running — or finds it done.
func (q *Queue) Cancel(ctx context.Context, id int64) (string, error) {
	var state string
	err := q.db.QueryRow(ctx, `
		UPDATE jobs
		SET cancel_requested = true,
		    state       = CASE WHEN state = 'queued' THEN 'done' ELSE state END,
		    exit_code   = CASE WHEN state = 'queued' THEN -1 ELSE exit_code END,
		    error       = CASE WHEN state = 'queued' THEN $2 ELSE error END,
		    finished_at = CASE WHEN state = 'queued' THEN now() ELSE finished_at END
		WHERE id = $1 AND state <> 'done'
		RETURNING state`, id, ReasonCancelled).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := q.Get(ctx, id); getErr != nil {
			return "", getErr
		}
		return "", ErrDone
	}
	if err != nil {
		return "", fmt.Errorf("queue: cancel: %w", err)
	}
	return state, nil
}

// Result is what a worker reports when the build has exited.
type Result struct {
	// ExitCode is the command's own status, or -1 if it was killed by a
	// signal or never ran.
	ExitCode int
	// OOMKilled is the sandbox's distinction: the kernel killed the build
	// for its memory limit, which looks like any other SIGKILL otherwise.
	OOMKilled bool
	// Error is set when the build did not run to an exit status of its own:
	// the sandbox failed to build, or the job was cancelled.
	Error string
	// Log is the build's stdout and stderr, interleaved.
	Log Log
}

// Report marks the job done with r. ErrLeaseLost means another worker holds
// the job now and this result is discarded: it may be a result of a build
// that was already killed, or of one that ran to completion after the
// lease was lost — at-least-once delivery, and Phase 4's to tighten.
func (q *Queue) Report(ctx context.Context, job Job, r Result) error {
	tag, err := q.db.Exec(ctx, `
		UPDATE jobs
		SET state = 'done', exit_code = $3, oom_killed = $4, error = $5,
		    log = COALESCE($6, ''::bytea), log_bytes = $7,
		    finished_at = now(), lease_until = NULL
		WHERE id = $1 AND fence = $2 AND state = 'running'`,
		job.ID, job.Fence, r.ExitCode, r.OOMKilled, r.Error, r.Log.Tail, r.Log.Total)
	if err != nil {
		return fmt.Errorf("queue: report: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Counts is how many jobs are in each state — the queue's depth, for a
// scrape.
func (q *Queue) Counts(ctx context.Context) (map[string]int64, error) {
	rows, err := q.db.Query(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("queue: counts: %w", err)
	}
	defer rows.Close()
	counts := map[string]int64{"queued": 0, "running": 0, "done": 0}
	for rows.Next() {
		var state string
		var n int64
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("queue: counts: %w", err)
		}
		counts[state] = n
	}
	return counts, rows.Err()
}

// Status is a job as the queue sees it.
type Status struct {
	State   string // queued, running or done
	Attempt int
	Worker  string
	// Cancelled is set once Cancel has been called; a running job stays
	// running until its worker notices.
	Cancelled bool
	// Result is complete once State is done. While the job runs, Result.Log
	// is what the worker has flushed so far.
	Result Result
}

// Get reads a job's state.
func (q *Queue) Get(ctx context.Context, id int64) (Status, error) {
	var s Status
	var exit *int
	err := q.db.QueryRow(ctx, `
		SELECT state, attempt, worker, cancel_requested, exit_code, oom_killed, error, log, log_bytes
		FROM jobs WHERE id = $1`, id).
		Scan(&s.State, &s.Attempt, &s.Worker, &s.Cancelled, &exit, &s.Result.OOMKilled, &s.Result.Error,
			&s.Result.Log.Tail, &s.Result.Log.Total)
	if errors.Is(err, pgx.ErrNoRows) {
		return Status{}, ErrNotFound
	}
	if err != nil {
		return Status{}, fmt.Errorf("queue: get: %w", err)
	}
	s.Result.ExitCode = -1
	if exit != nil {
		s.Result.ExitCode = *exit
	}
	return s, nil
}
