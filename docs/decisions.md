# Decisions

One entry per decision that cost something to make. Dated, with the alternative
that was rejected and why.

## 2026-09-08 — Zero dependencies until one is unavoidable

`go.mod` has no `require` block. Everything so far (pool, signal handling,
structured logging via `log/slog`) is standard library.

Rejected: `go.uber.org/goleak` for the leak test. It is the better tool, but the
leak assertion here is a `runtime.NumGoroutine()` comparison with a bounded poll
— about fifteen lines — and a dependency added in week one is a dependency
audited forever. Revisit if leak detection gets subtle enough that the poll
produces false results.

`pgx`, `grpc-go`, and the OpenTelemetry SDK are pre-approved by the project
brief and will arrive with the phases that need them.

## 2026-09-08 — Drain and cancel are different shutdowns

The pool exposes `Close()` (stop accepting, finish accepted work) and root
context cancellation (stop now, propagate into running tasks) as separate
mechanisms rather than one `Shutdown(ctx)`.

Why: Kubernetes will send SIGTERM to a worker holding a job lease. The correct
response is drain — finish the build, report the result, then exit — and only
escalate to cancel when the grace period runs out. Collapsing both into one call
would make that escalation impossible to express. `cmd/forge-worker` already
wires it as first signal = drain, second signal = cancel.

## 2026-09-08 — Development on macOS

The development machine is macOS/arm64. Namespaces, cgroups v2, `pivot_root`,
and seccomp do not exist there, and faking them would make the tests worthless.

Plan for Week 2 onward:

- Portable packages (`pool`, later `queue`, `cas`, API handlers) build and test
  on macOS and are covered by CI.
- Sandbox code lives behind `//go:build linux` and its tests are Linux-only. They
  are **not** skipped silently on other platforms — the build tag means they do
  not compile there, and CI runs on `ubuntu-latest` so they always execute
  somewhere.
- Local Linux kernel via `colima` (already installed) or `lima`, running an
  arm64 VM with cgroups v2. Docker inside that VM is allowed for two things
  only: exporting an Alpine rootfs, and running PostgreSQL. It is never the
  execution runtime.

Rejected: developing the sandbox against a Docker container on macOS as if it
were a host. The container already has its own namespaces; testing isolation
from inside one measures the wrong thing.

## Pending decisions

These are called out in the brief and will be written up when the code that
forces them lands: PostgreSQL vs Redis as the queue, leases vs Raft, custom
runtime vs runc/containerd, the Kubernetes privilege model for workers, cache
key composition, and queue-depth vs CPU autoscaling.
