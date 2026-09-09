# Forge — architecture

Status: **Phase 0, Week 3.** The bounded worker pool, the sandbox's namespace
isolation with a private device filesystem, and per-job cgroups v2 resource
limits exist. This document describes the target shape and is
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
     |  mount(tmpfs, /dev) + mknod x6    six devices, and nothing else
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

**A private `/dev`.** The sandbox mounts a tmpfs on `/dev` and creates six
character devices on it — `null`, `zero`, `full`, `random`, `urandom`, `tty` —
plus `fd`, `stdin`, `stdout` and `stderr` as symlinks into its own `/proc`. The
host's `/dev` is never consulted: the nodes are made from the major and minor
numbers in the kernel's device registry, so the set is exactly what the code
lists and nothing depends on what the host happens to have.

The tmpfs is what makes it correct rather than merely present. The rootfs is
bind-mounted and shared by every build on the worker, so without a mount over
it, `dev/` is a writable directory in that shared image: a build redirecting to
`/dev/null` creates a *file* of that name, which the next build inherits.
Reads (`/dev/urandom`) failed loudly; writes succeeded and contaminated. With
the tmpfs, both are correct and neither outlives the mount namespace, so there
is no cleanup path to get wrong.

**Not isolated yet**, and named as such in the README rather than implied away:
no user namespace (sandbox root *is* host root, and keeps `CAP_MKNOD`, so a
hostile build can still make its own device nodes), no seccomp filter, and no
network interface at all. Resource limits landed in Week 3; the rest is
Phase 1.

Tests live behind `//go:build linux` and need `CAP_SYS_ADMIN` plus an unpacked
rootfs; `make test-sandbox` on Linux, `make vm-test` from macOS via colima, and
CI runs the former on every push.

### internal/cgroup — cgroups v2 resource limits

One cgroup per job, created before it starts and removed after it exits. The
job is placed in it by the kernel at clone time, not moved in afterwards:
`Run` passes the cgroup's directory descriptor as `SysProcAttr.CgroupFD`, so
`clone3(CLONE_INTO_CGROUP)` puts the process there before its first
instruction. There is no window in which the build runs uncapped.

```
                  /sys/fs/cgroup/forge          <- Spec.CgroupRoot
                    cgroup.subtree_control: +cpu +memory +pids
                        |
                        +-- job-1837462          <- one build
                        |     memory.max      67108864
                        |     memory.swap.max 0
                        |     cpu.max         50000 100000
                        |     pids.max        32
                        |
                        +-- job-2910375          <- another, concurrently
```

| Limit | File | What happens at the cap |
|---|---|---|
| `MemoryBytes` | `memory.max` | Reclaim, then an OOM kill inside the cgroup. |
| — | `memory.swap.max` | Pinned to 0 whenever memory is capped. |
| `CPUCores` | `cpu.max` | Throttled, never killed. |
| `MaxPIDs` | `pids.max` | `fork` fails; the build sees the error. |

A zero field writes no file — a fresh cgroup is already unlimited, so the
absence of a limit and the string `max` mean the same thing.

**Reporting an OOM kill as an OOM kill.** The kernel's OOM kill reaches
userspace as SIGKILL, indistinguishable from a lease timeout or an operator's
`kill -9`; through a shell all four are exit 137. `Run` therefore reads
`memory.events` between `wait4` returning and the cgroup being removed — the
one moment the counter is both final and still readable — and wraps the error
with `ErrOOMKilled`. The error still unwraps to `*exec.ExitError`, so a caller
that only knows about exit codes loses nothing.

**Cleanup.** `Close` removes the directory, retrying while the kernel is still
reaping the job's processes: `rmdir` on a cgroup with members fails with EBUSY,
and the last task's exit is not synchronous with the parent's wait returning. A
cgroup that is never removed is a kernel object leaked once per build.

`Spec.CgroupRoot` is a parameter because under systemd the worker is handed a
delegated subtree and must stay inside it; passing that path is the whole
adaptation. The default is `/sys/fs/cgroup/forge`.

## Next

Phase 0 Week 4 — seccomp: a syscall filter applied after the sandbox is built
and before the build command is exec'd, denying by default rather than listing
what to block. It is also what would bound `mknod`, which the private `/dev`
deliberately does not. Then the user namespace that makes sandbox root stop being host
root, which is the change that turns this from a boundary against accident into
one against a hostile build.
