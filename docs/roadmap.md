# Roadmap — what "done" means

Forge is done when the first sentence of the README is true end to end: a
job submitted over an API is scheduled onto a worker and runs inside the
sandbox, and a worker that dies mid-job does not lose it or run it twice.
Five phases; four are complete.

| Phase | Delivers | State |
|---|---|---|
| 0 | The sandbox's primitives: worker pool, namespaces + `pivot_root`, cgroups v2, `/dev`, seccomp, user namespace | done — [phase-0.md](phase-0.md) |
| 1 | The sandbox finished: per-job overlay, `clone` flags bounded, capability bounding set | done — [phase-1.md](phase-1.md) |
| 2 | **A worker that claims jobs from a durable queue** and runs them in the sandbox | done — [phase-2.md](phase-2.md) |
| 3 | **An API** that submits jobs, reports their state and streams their logs | done — [phase-3.md](phase-3.md) |
| 4 | **More than one worker**: leases that expire, fencing that makes a late worker's report harmless, a job that survives its worker | done — [phase-4.md](phase-4.md) |
| 5 | **Operable**: metrics, a deployment document, and benchmark numbers from a harness | next |

## Phase 2 — worker and queue

The target shape in [architecture.md](architecture.md) puts the queue in
PostgreSQL — durable state and the queue in one place, no Redis, no Kafka —
claimed with `FOR UPDATE SKIP LOCKED`. That is also Forge's first
third-party dependency, which `go.mod` has so far had none of. **This is the
decision Phase 2 waits on**, and it is Parth's:

- `jackc/pgx/v5` — the current standard driver, pure Go, no cgo. The
  obvious choice if PostgreSQL is the store.
- `database/sql` + `lib/pq` — in maintenance mode upstream; not recommended.
- SQLite (`modernc.org/sqlite`, pure Go) for a single-node Phase 2, with
  PostgreSQL in Phase 4 — defers the dependency but not the decision, and
  the claim query is written twice.

Once decided, Phase 2 is:

1. `internal/queue`: the schema (`jobs`, `runs`, `leases`), `Claim`,
   `Heartbeat`, `Report`, each a single transaction; tested against a real
   PostgreSQL in CI (`services:` in the workflow), never a mock.
2. `forge-worker` becomes real: a `pool.Pool` of job loops, each claiming,
   running `sandbox.Run`, heartbeating on a ticker, reporting the exit
   status and the OOM distinction the sandbox already makes.
3. Logs: captured to the run row (bounded) — streaming is Phase 3's.
4. Definition of done in `phase-2.md`, the same shape as 0 and 1, before
   the first commit of code.

## Phase 3 — API

`forge-api`: `POST /jobs`, `GET /jobs/{id}`, `GET /jobs/{id}/logs` as a
stream. Standard library `net/http` only. Webhook ingestion (GitHub) is
part of the target shape but not of "done"; it is a client of `POST /jobs`.

## Phase 4 — more than one worker

The distributed part, and the part where the queue's design is tested
rather than described: lease expiry reclaims a job from a dead worker; a
fencing token on every report means the dead worker coming back cannot
overwrite the live one's result; a job is never run twice to completion.
Tests kill workers mid-job. This is where `sandbox.Run`'s context
cancellation and `Pdeathsig` earn their keep.

## Phase 5 — operable

Prometheus metrics from worker and API, a deployment document (systemd
units, the cgroup delegation the README already names, the image shift),
and benchmark numbers — jobs per second per worker, sandbox setup latency
— produced by a harness in the repo, or not quoted.

## Carried from the sandbox

Named in [phase-1.md](phase-1.md): `/dev/ptmx` and `/dev/shm` when a build
needs them; artifacts — nothing a build writes survives the job today, and
the persistent upper layer Phase 1 rejected is where a later phase collects
from; `Run` discarding `group.Close()`'s error.
