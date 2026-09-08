# Forge — architecture

Status: **Phase 0, Week 1.** Only the bounded worker pool exists. This document
describes the target shape and is updated as each milestone lands; anything not
marked *implemented* is not built yet.

## Target shape

```
client / GitHub webhook
        |
        v
   forge-api  ──────────────►  PostgreSQL (jobs, runs, leases, fencing tokens)
        ▲                            ▲
        │ gRPC (claim, heartbeat,    │ FOR UPDATE SKIP LOCKED
        │       report, logs)        │
        ▼                            │
   forge-worker ─────────────────────┘
        |
        | fork/exec of /proc/self/exe child
        v
     forged  (namespaces, pivot_root, cgroups v2, seccomp, veth)
        |
        v
   untrusted build command
```

- **forge-api** — HTTP API, job submission, log fan-out, webhook ingestion.
- **forge-worker** — claims jobs, holds leases, drives the sandbox, streams logs.
- **forged** — the sandbox runtime. Re-executes itself as `child`, applies
  isolation, runs the build command as PID 1 of its own PID namespace.
- **PostgreSQL** — durable state *and* the queue. No Redis, no Kafka.

## Implemented

### internal/pool — bounded worker pool

`Pool` runs `Task`s on a fixed set of goroutines. It is the concurrency
primitive the worker will use for its job loop, so its shutdown semantics are
the point of the package, not the fan-out.

Two distinct shutdown modes:

| Mode | Trigger | Effect |
|---|---|---|
| Drain (graceful) | `Close()` | Stop accepting; queued and running tasks finish. |
| Cancel (hard) | root `ctx` cancelled | Workers stop; running tasks see `ctx.Done()`. |

The one subtle part is that `Close` closes the task channel while `Submit` may
be parked on a send to it — a "send on closed channel" panic waiting to happen.
`Submit` therefore registers itself in a `sync.WaitGroup` under a read lock
before sending; `Close` flips the closed flag under the write lock, waits for
every registered sender to leave, and only then closes the channel. A sender
parked on a full queue is never stuck, because its `select` also watches the
caller's context and the pool's root context.

Worker loop re-checks `ctx.Err()` after receiving a task: `select` picks
randomly among ready cases, so a cancelled pool with a non-empty queue would
otherwise keep starting new work.

## Next

Phase 0 Week 2 — `forged` re-exec skeleton and namespace isolation. Requires a
Linux kernel; see [decisions.md](decisions.md#development-on-macos).
