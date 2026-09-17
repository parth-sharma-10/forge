# Phase 3 — an API that submits jobs, reports them and streams their logs

`forge-api` is the front door: a job goes in over HTTP, its state comes back
over HTTP, and its output can be followed while it runs. Standard library
`net/http` and nothing else — the dependency count stays at one.

## Deliverables

| # | Deliverable | Proven by |
|---|---|---|
| 1 | `POST /jobs` with `{"args": [...]}` → `201 {"id": n}` and a `Location`; a bad body or an empty command is `400` | `TestSubmitAndGet`, `TestBadRequests` |
| 2 | `GET /jobs/{id}` → the job's state, attempt, worker, exit code, OOM flag, error, and how many bytes of log it has produced; `404` for an unknown id | `TestSubmitAndGet`, `TestUnknownJobIs404` |
| 3 | `GET /jobs/{id}/logs` streams the output as the worker produces it and ends when the job does; a job already done returns its log at once | `TestLogsStreamWhileTheJobRuns`, `TestLogsOfADoneJobReturnAtOnce` |
| 4 | The worker flushes its log tail to the queue with every heartbeat, and heartbeats every 2s or a third of the lease, whichever is sooner, so a log is at most that stale | `TestHeartbeatCarriesTheLog` (queue), `TestTheWorkerFlushesTheLogWhileRunning` (worker) |
| 5 | Truncation is visible: the row holds a tail, the total byte count is kept beside it, and a follower that missed bytes is told how many; a job restarted by another worker is announced rather than spliced | `TestLogsSayWhenBytesWereSkipped`, `TestLogsSayWhenTheJobRestarted` |
| 6 | `forge-api` binary: `-dsn`, `-listen` | `make build`; the README's worked example |

## Decisions made here, recorded in decisions.md

- **Logs ride the heartbeat.** The worker already writes the row every few
  seconds to keep its lease; the log tail goes in the same statement. No
  log table, no second connection, no second goroutine.
- **Following is polling.** `GET /logs` reads the row every half second and
  writes what is new. LISTEN/NOTIFY would cut the latency; nothing needs it
  cut, and it would be a second code path to keep the first one honest.
- **JSON in, JSON out, text for logs.** No content negotiation.
- **The handler tests use a store in memory.** The handler's own logic —
  the wire format, the follow loop, the truncation arithmetic — is what
  they test; the queue's contract is proven in its own package against
  PostgreSQL, and the README's example is the two together.
- **Found on the way, fixed here:** the API and the worker starting together
  on a fresh database raced on `CREATE TABLE IF NOT EXISTS`, which is not
  safe against itself, and killed the worker; schema application now runs
  under an advisory lock. And `Open` closing the pool with a failed
  transaction still open hung instead of returning the error.

## Not in Phase 3

- Authentication. The API trusts its network, and says so in the README.
- Webhook ingestion. It is a client of `POST /jobs` and can live anywhere.
- Cancelling a job, listing jobs, deleting jobs.

## Exit criteria

All three held on 2026-09-16 at the commit this file was last changed in.

1. **CI green on x86_64**, the API tests, the queue tests against the
   service container and the worker's end-to-end all running, none skipped.
2. **`go.mod` still requires exactly one module.**
3. **The README shows a job submitted with `curl`, followed with `curl`, and
   read back** — output pasted from the run, the three log lines a second
   apart as they were written.
