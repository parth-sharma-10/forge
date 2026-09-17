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

// ErrNotFound is returned by Get for an id that was never submitted.
var ErrNotFound = errors.New("queue: no such job")

// schema is applied by Open. CREATE IF NOT EXISTS rather than a migration
// tool: there is one table, and the day it needs a second shape is the day
// a migration tool earns its place.
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
	created_at  timestamptz NOT NULL DEFAULT now(),
	started_at  timestamptz,
	finished_at timestamptz
);
CREATE INDEX IF NOT EXISTS jobs_claimable ON jobs (id) WHERE state <> 'done';
`

// Queue is a handle on the jobs table. It is safe for concurrent use.
type Queue struct {
	db *pgxpool.Pool
}

// Open connects to PostgreSQL and makes sure the table exists.
func Open(ctx context.Context, dsn string) (*Queue, error) {
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	if _, err := db.Exec(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("queue: apply schema: %w", err)
	}
	return &Queue{db: db}, nil
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

// Job is a claimed job: what to run, and the token that proves the claim.
type Job struct {
	ID         int64
	Args       []string
	Fence      int64
	LeaseUntil time.Time
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
		RETURNING id, args, fence, lease_until`,
		worker, lease).Scan(&j.ID, &j.Args, &j.Fence, &j.LeaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrEmpty
	}
	if err != nil {
		return Job{}, fmt.Errorf("queue: claim: %w", err)
	}
	return j, nil
}

// Heartbeat extends the lease by lease from now. ErrLeaseLost means the
// job was reclaimed: the worker should kill the build.
func (q *Queue) Heartbeat(ctx context.Context, job Job, lease time.Duration) error {
	tag, err := q.db.Exec(ctx, `
		UPDATE jobs SET lease_until = now() + $3
		WHERE id = $1 AND fence = $2 AND state = 'running'`,
		job.ID, job.Fence, lease)
	if err != nil {
		return fmt.Errorf("queue: heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
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
	// Log is the tail of the build's stdout and stderr.
	Log []byte
}

// Report marks the job done with r. ErrLeaseLost means another worker holds
// the job now and this result is discarded: it may be a result of a build
// that was already killed, or of one that ran to completion after the
// lease was lost — at-least-once delivery, and Phase 4's to tighten.
func (q *Queue) Report(ctx context.Context, job Job, r Result) error {
	tag, err := q.db.Exec(ctx, `
		UPDATE jobs
		SET state = 'done', exit_code = $3, oom_killed = $4, error = $5, log = COALESCE($6, ''::bytea),
		    finished_at = now(), lease_until = NULL
		WHERE id = $1 AND fence = $2 AND state = 'running'`,
		job.ID, job.Fence, r.ExitCode, r.OOMKilled, r.Error, r.Log)
	if err != nil {
		return fmt.Errorf("queue: report: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Status is a job as the queue sees it.
type Status struct {
	State   string // queued, running or done
	Attempt int
	Worker  string
	Result  Result // meaningful once State is done
}

// Get reads a job's state.
func (q *Queue) Get(ctx context.Context, id int64) (Status, error) {
	var s Status
	var exit *int
	err := q.db.QueryRow(ctx, `
		SELECT state, attempt, worker, exit_code, oom_killed, error, log
		FROM jobs WHERE id = $1`, id).
		Scan(&s.State, &s.Attempt, &s.Worker, &exit, &s.Result.OOMKilled, &s.Result.Error, &s.Result.Log)
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
