# Forge — architecture

Status: **Phase 0, Week 2.** The bounded worker pool and the sandbox's
namespace isolation exist. This document describes the target shape and is
updated as each milestone lands; anything not marked *implemented* is not built
yet.

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

### internal/sandbox — namespace isolation and pivot_root

`Run` executes one command inside a fresh sandbox. The isolation cannot be
applied to the calling process — `CLONE_NEWPID` only affects children, and
`pivot_root` in place would take the rest of the program with it — so `Run`
re-executes `/proc/self/exe` with the namespace flags on the clone, and that
copy isolates itself before `exec`ing the build command over itself. Callers
call `sandbox.MaybeChild()` first thing in `main` so the re-executed copy takes
the child path.

```
forge-worker (or forged, or the test binary)
     |
     | clone(CLONE_NEWNS|NEWUTS|NEWIPC|NEWPID|NEWNET), Pdeathsig=SIGKILL
     v
/proc/self/exe forge-sandbox-child <argv...>
     |  sethostname
     |  mount(/, MS_REC|MS_PRIVATE)      nothing propagates back to the host
     |  mount(rootfs, rootfs, MS_BIND)   pivot_root needs a mount point
     |  pivot_root(rootfs, .forge-oldroot)
     |  umount2(old, MNT_DETACH); rmdir  no mount left to walk back to
     |  mount(proc, /proc)               the new PID namespace's process table
     v
execve(build command)                    PID 1 of its own namespace
```

| Namespace | What it costs an escaping build |
|---|---|
| mount | No path to the host filesystem; mounts it makes die with it. |
| uts | Its own hostname; cannot rename the node. |
| ipc | No shared memory or message queues with anything on the host. |
| pid | Sees only itself; cannot signal a host process. |
| net | No interfaces beyond loopback, so no network at all yet. |

Two properties fall out of the shape rather than being coded for. Cancelling the
context kills PID 1 of the namespace, and the kernel then kills everything else
in it, so a build cannot outlive its context by forking away. `Pdeathsig` means
a worker that dies mid-build does not leave the build running.

Exit status 125 distinguishes "the sandbox could not be built" from the build
command's own status, which is otherwise passed through untouched.

**Not isolated yet**, and named as such in the README rather than implied away:
no user namespace (sandbox root *is* host root), no cgroup limits, no seccomp
filter, no `/dev`, and no network interface at all. Those are Phase 1.

Tests live behind `//go:build linux` and need `CAP_SYS_ADMIN` plus an unpacked
rootfs; `make test-sandbox` on Linux, `make vm-test` from macOS via colima, and
CI runs the former on every push.

## Next

Phase 0 Week 3 — cgroups v2: a per-job cgroup with memory and CPU limits,
written through the delegated subtree rather than by the sandbox reaching into
`/sys/fs/cgroup` itself, and an OOM kill that is reported as an OOM kill rather
than as exit 137. Then seccomp, and the user namespace that makes sandbox root
stop being host root.
