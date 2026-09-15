# Forge — architecture

Status: **Phase 0 complete** — see [phase-0.md](phase-0.md) for what that
means and how it is checked. The bounded worker pool, the sandbox's namespace
isolation with a private device filesystem, per-job cgroups v2 resource limits,
a deny-by-default seccomp filter and the user namespace exist. This document
describes the target shape and is updated as each milestone lands; anything
not marked *implemented* is not built yet.

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
     | clone(CLONE_NEWUSER|NEWNS|NEWUTS|NEWIPC|NEWPID|NEWNET), Pdeathsig=SIGKILL
     |   uid/gid map 0..65535 -> 524288.., then setgid(0)/setuid(0)
     v
/proc/self/exe forge-sandbox-child <argv...>   root inside, uid 524288 outside
     |  sethostname
     |  mount(/, MS_REC|MS_PRIVATE)      nothing propagates back to the host
     |  mount(rootfs, rootfs, MS_BIND)   pivot_root needs a mount point
     |  mount(proc, rootfs/proc)         the new PID namespace's process table
     |  mount(tmpfs, rootfs/dev)         + six bind mounts from the host's /dev
     |  pivot_root(".", ".")             the old root parks on top of the new
     |  umount2(".", MNT_DETACH)         no mount left to walk back to
     v
execve(build command)                    PID 1 of its own namespace
```

Both mounts happen before `pivot_root`, and neither is free to move: the
device nodes are bind mounts of the host's, which after `pivot_root` is not a
path any more, and a fresh `proc` may only be mounted from a user namespace
while a fully visible `proc` is still in the mount namespace — which the detach
of the old root takes away. See `docs/failures.md`, 2026-09-11.

| Namespace | What it costs an escaping build |
|---|---|
| user | Its root is host uid 524288: it owns nothing on the host and its capabilities stop at the namespace's edge. |
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

**A private `/dev`.** The sandbox mounts a tmpfs on `/dev` and puts six
character devices on it — `null`, `zero`, `full`, `random`, `urandom`, `tty` —
plus `fd`, `stdin`, `stdout` and `stderr` as symlinks into its own `/proc`.
Each node is a bind mount of the host's, because a process in a user namespace
cannot create a device node at all: `mknod` for a character device wants
`CAP_MKNOD` in the *initial* namespace, which is the one privilege the sandbox
has given up. The major and minor numbers from the kernel's device registry are
still in the code, now as an assertion — `/dev/null` is bound in only after the
host's is confirmed to be character device 1:3. What has not changed is that
the host's `/dev` is never mounted in wholesale; the set is exactly those six.

The tmpfs is what makes it correct rather than merely present. The rootfs is
bind-mounted and shared by every build on the worker, so without a mount over
it, `dev/` is a writable directory in that shared image: a build redirecting to
`/dev/null` creates a *file* of that name, which the next build inherits.
Reads (`/dev/urandom`) failed loudly; writes succeeded and contaminated. With
the tmpfs, both are correct and neither outlives the mount namespace, so there
is no cleanup path to get wrong.

**Sandbox root is not host root.** `CLONE_NEWUSER` maps uid and gid 0..65535
inside to 524288..589823 outside, and `Credential` moves the child onto the
first of them — the map alone would be decoration, since a child cloned from
root keeps the root it was cloned with. The build still sees itself as uid 0
and still holds `CAP_SYS_ADMIN` *in its own namespaces*, which is what lets it
mount and `pivot_root`; on the host's side of the boundary it owns nothing. A
consequence worth stating: the rootfs image belongs to host root, which the map
does not cover, so the image is no longer writable from inside — `/tmp` is
world-writable and is the only part a build can still contaminate.

**Not isolated yet**, and named as such in the README rather than implied away:
every job on a worker shares one map, so this is a boundary between a build and
the host rather than between two builds, and there is no network interface at
all. Resource limits landed in Week 3, the syscall filter in Week 4, the user
namespace in Week 5.

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

### internal/sandbox — the seccomp filter (implemented, Week 4)

A classic BPF program, installed by the child on itself as the last thing
before `execve` and inherited across it. The kernel runs it on every syscall
against a `struct seccomp_data`; this one reads two fields of that struct and
nothing else.

**An allowlist, not a blocklist.** The filter names the syscalls a build is
expected to need — 157 that exist on every architecture Forge builds for, plus
that architecture's own (192 in total on amd64, 159 on arm64, which has no
legacy `open`/`stat`/`fork` forms) — and denies everything else — so a syscall a future kernel
adds is denied by never having been listed, rather than by someone remembering
to add it to a blocklist. `mount`, `pivot_root`, `mknod`, `ptrace`, `bpf`,
`init_module`, `kexec_load`, `keyctl`, `perf_event_open`, `unshare` and the
rest of the kernel-facing surface are absent from the list and appear nowhere
in the implementation.

**Denials return `ENOSYS`, not `EPERM`.** See docs/decisions.md; the short
version is that `ENOSYS` is the one errno a C library treats as "fall back to
the older syscall", so an allowlist that returns anything else breaks every
program built against a newer libc than the list.

**Ordering.** Installed after the mounts, because building the sandbox uses
the syscalls the sandbox must not have — `mount` and `pivot_root` are
simultaneously what a build must not reach and what putting a build behind
this boundary requires. And after `exec.LookPath`, which walks `PATH` and has
no reason to run under the filter.

**Two details that are not incidental:**

- *The architecture is checked before the number.* Under the x32 or i386 ABI
  the same numbers name different syscalls, so a filter that compares numbers
  without pinning `seccomp_data.arch` is checking the wrong table. A
  mismatched ABI is killed, not denied: it is not something a build does by
  accident.
- *`SECCOMP_FILTER_FLAG_TSYNC`.* Go moves goroutines between OS threads, and
  `seccomp(2)` without TSYNC filters only the calling thread — so the thread
  that installs the filter is not guaranteed to be the thread that reaches the
  `execve`. TSYNC applies it to every thread of the process.

Syscall numbers come from Go's own `syscall` package, which resolves them per
architecture at compile time. The handful that package's frozen Linux tables
omit — `seccomp` and `getrandom` on amd64 — are written out in
`seccomp_linux_amd64.go`, beside the audit token for that architecture. An
architecture with no such file is a compile error rather than a filter built
from another architecture's numbers.

## Next

Phase 0's five named weeks are done: the worker pool, the sandbox, cgroup
limits, the private `/dev`, the seccomp filter and the user namespace. What is
still open is written down rather than implied — a per-job rootfs, so `/tmp` is
not shared between builds and one map is not shared by every job on a worker;
argument filtering for `clone`, whose flags the number-only filter cannot
bound; and the capability bounding set. [phase-0.md](phase-0.md) is the
definition of done and carries that list.
