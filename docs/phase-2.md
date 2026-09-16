# Phase 2 — a worker that claims jobs from a durable queue

The first thing above the sandbox. A job is a row in PostgreSQL; a worker
claims one, runs it in the sandbox, and writes back what happened. Written
before the first line of code, as [phase-0.md](phase-0.md) and
[phase-1.md](phase-1.md) were.

## Deliverables

| # | Deliverable | Proven by |
|---|---|---|
| 1 | `internal/queue`: `Submit`, `Claim`, `Heartbeat`, `Report`, `Get` over one table, each one statement; `Claim` is `FOR UPDATE SKIP LOCKED` and takes an expired lease as well as a queued job | `queue_test.go` against a real PostgreSQL — never a mock — `TestClaimIsExclusive` with concurrent claimers, `TestAnExpiredLeaseIsReclaimed`, `TestAStaleFenceCannotReportOrHeartbeat` |
| 2 | A fencing token on every claim: a worker whose lease was taken cannot heartbeat or report | `TestAStaleFenceCannotReportOrHeartbeat` |
| 3 | `internal/worker`: N loops that claim, run `sandbox.Run`, heartbeat on a ticker, and report the exit status and the OOM distinction; a lost lease kills the job in hand; drain and cancel are different stops | `worker_test.go` with a fake store and a fake runner: `TestAJobIsRunAndReported`, `TestALostLeaseKillsTheJob`, `TestDrainFinishesTheJobInHandAndClaimsNoMore`, `TestCancelKillsTheJobInHand` |
| 4 | End to end: a submitted job runs in the real sandbox on a real queue, and its exit status, log and OOM kill come back | `TestEndToEnd*` in `internal/worker`, root + rootfs + PostgreSQL — the CI `test-sandbox` step |
| 5 | `forge-worker` is the real thing: `-dsn`, `-rootfs`, `-workers`, `-lease`, and the sandbox's limit flags | `make build`; the README's worked example |
| 6 | The first dependency, `jackc/pgx/v5`, and nothing else | `go.mod` |

## Decisions made here, recorded in decisions.md

- One table, `jobs`, not three. A run is an attempt on the job row; the
  history of attempts is Phase 4's to want, if it wants it.
- `Claim` reclaims an expired lease in the same statement that claims a
  queued job. Not Phase 4 pulled forward: it is the same `WHERE` clause,
  and a lease that nothing reclaims is a timestamp, not a lease.
- Delivery is at-least-once. A worker that dies after the build and before
  the report leaves a job to be run again. Phase 4 measures it; nothing in
  Phase 2 pretends otherwise.
- No attempt cap. A job that kills its worker every time is reclaimed
  forever — the ceiling is named with a `ponytail:` comment, and Phase 4
  owns it.
- Logs are the last `LogLimit` bytes of stdout and stderr interleaved, on
  the job row. Streaming is Phase 3's.
- The queue tests skip without `FORGE_TEST_DSN` on a developer's machine
  and **fail** without it under `CI=true`, so a skipped test cannot pass
  the gate.

## Not in Phase 2

- An API. Jobs are submitted by `queue.Submit` — from tests, and from
  `psql` in the README's example — until Phase 3.
- More than one worker process against the same queue, and the tests that
  kill one mid-job. The queue is written for it; Phase 4 proves it.
- Artifacts, a `runs` table, an attempt cap, log streaming.

## Exit criteria

1. CI green on x86_64: `make test` with the queue tests running against
   the service container, and `make test-sandbox` with the end-to-end
   test running, neither skipped.
2. `make test` locally against a scratch PostgreSQL: 0 failures.
3. `go.mod` requires exactly one module.
4. The README shows a job submitted, claimed and reported, with real output.
