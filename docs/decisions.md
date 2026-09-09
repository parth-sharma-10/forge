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
  execution runtime. (The rootfs half of that was superseded on 2026-09-09 —
  see "The rootfs is a pinned tarball" below.)

Rejected: developing the sandbox against a Docker container on macOS as if it
were a host. The container already has its own namespaces; testing isolation
from inside one measures the wrong thing.

## 2026-09-09 — Re-exec `/proc/self/exe`, not `unshare` in place

The sandbox is applied by starting a second copy of the running binary with the
namespace flags on the `clone`, and having that copy isolate itself and `exec`
the build command over itself. Callers opt in by calling `sandbox.MaybeChild()`
as the first statement of `main`.

Why it cannot be done in place: `CLONE_NEWPID` only takes effect for *children*
of the calling process. A process cannot move itself into a new PID namespace,
so there is no arrangement of `unshare(2)` calls that puts the build command at
PID 1 without a fork in between. The mount namespace argues the same way from
the other side — `pivot_root` in the current process would take the rest of the
program's filesystem with it.

Rejected: shipping a separate `forge-sandbox-helper` binary and exec'ing that.
It is the more obvious design, and it means the worker and the helper can drift
to different versions on a partially-rolled-out node, with the sandbox
semantics silently mismatched against the worker that thinks it knows them.
`/proc/self/exe` is by construction the same build.

The cost of the chosen design is real and worth naming: every binary that calls
`Run` must call `MaybeChild` before parsing flags, or it will re-run its own
`main` with Forge's argv. The test binary is subject to the same rule, which is
also what lets the tests exercise the real re-exec path rather than a stub.

## 2026-09-09 — `pivot_root`, not `chroot`

The child bind-mounts the rootfs onto itself, `pivot_root`s into it, then
detaches the old root with `MNT_DETACH` and removes the mountpoint.

`chroot` alone is not a boundary: it moves the path resolution root but leaves
the old tree mounted and reachable. A process holding a directory file
descriptor from before the call — or one that regains `CAP_SYS_CHROOT` — walks
back out with `fchdir` and `..`. `pivot_root` plus a detach leaves no mount to
walk back to, which is why the detach is not cleanup but part of the isolation.

Ordering is load-bearing and is documented on `child()`: private propagation
before the bind (or the bind escapes to the host, and `pivot_root` refuses a
shared root), the bind before `pivot_root` (or it refuses a non-mount-point with
EBUSY), the detach before `/proc` is mounted (so `/proc` is the new PID
namespace's process table rather than a copy of the host's). Each of those four
steps has a mutation test behind it.

## 2026-09-09 — No user namespace yet, so sandbox root is host root

`CLONE_NEWUSER` is deliberately not in the set. Everything runs as real root,
which means `Run` needs `CAP_SYS_ADMIN` and a process that escapes the sandbox
is root on the host.

Why not now: a user namespace changes what every other primitive means — which
uid owns the rootfs files, whether `mount` is permitted, how cgroup delegation
is written, what seccomp has left to deny. Adding it after those exist means
reworking them; adding it before them means writing them against a moving
target. It lands in Phase 1 as its own change, with the uid/gid mapping and the
"container root is uid 100000 on the host" property tested directly.

Until then the sandbox is an isolation boundary against *accident*, not against
a hostile build. `README.md` says so, and it will keep saying so until the
mapping is real.

## 2026-09-09 — The rootfs is a pinned tarball, not `docker export`

`make rootfs` downloads the Alpine minirootfs tarball for the host architecture
and checks it against a SHA-256 pinned in the Makefile.

Rejected: `docker export $(docker create alpine)`, which the Week 2 plan
assumed. It needs a working container runtime on every machine that runs the
tests — including CI and the colima VM, whose runtime is containerd, not Docker
— to produce a filesystem the tarball already is. The checksum is pinned in the
repo rather than fetched from the mirror's own `.sha256`, because a mirror that
can serve a modified tarball can serve a matching checksum next to it. This is
code Forge unpacks and executes as root.

## 2026-09-09 — Exit status 125 means the sandbox failed, not the build

The child exits 125 when it cannot build the sandbox, and the reason goes to
stderr. Any other status is the build command's own, passed through untouched.

Without the split, a build that legitimately exits 1 and a sandbox that could
not mount `/proc` are the same event to the caller — and the scheduler that
decides whether to retry a job on another worker needs to tell "this build
fails" from "this worker cannot run builds". 125 follows the convention `docker
run` and `env` already use, so it is one fewer thing to explain.

## 2026-09-09 — Testing against a colima VM, from the macOS host

`make vm-test` cross-compiles the test binary on the host and runs it under
`sudo` inside the colima VM. `make test-sandbox` is the same thing natively, and
is what CI runs on `ubuntu-latest`.

colima mounts `$HOME` into the VM at the same path, so the binary needs no
copying in and the VM needs no Go toolchain — which also means the VM is
disposable, and nothing about the tests depends on state inside it beyond the
unpacked rootfs.

The sandbox tests skip rather than fail when run unprivileged or without a
rootfs, which is a real hazard: an all-skipped run reports `PASS`. That is why
`test-sandbox` sets the rootfs itself, passes `-test.v` so the skips are visible
in CI logs, and depends on the `rootfs` target rather than trusting the
environment.

## 2026-09-09 — The job is born in its cgroup, not moved into it

`Run` sets `SysProcAttr.UseCgroupFD` with a descriptor for the job's cgroup, so
the kernel places the process there as part of `clone3(CLONE_INTO_CGROUP)`.

Rejected: the conventional sequence of starting the process and then writing its
pid to `cgroup.procs`. Between those two steps the build is running with no
limits at all. It is a small window, and a build that wants to allocate a few
gigabytes before anyone stops it only needs a small window. Every workaround for
it — a synchronisation pipe the child blocks on, a stopped process resumed after
the write — is more code than not having the window.

The cost is a hard floor of Linux 5.7 for `CLONE_INTO_CGROUP` and Go 1.22 for
`UseCgroupFD`. Go does not silently fall back if `clone3` is unavailable; the
call fails, which is the right direction for a security control to fail in.

## 2026-09-09 — An OOM kill is read from the cgroup, not inferred from 137

`Group.OOMKilled` reads `memory.events` after the job exits, and `Run` wraps the
error with `ErrOOMKilled` when it says so.

The exit status cannot answer the question. An OOM kill arrives as SIGKILL,
which is also what a lease timeout, an operator's `kill -9`, and a parent's
`Pdeathsig` look like. "Exit 137" is four different incidents wearing the same
number, and the scheduler's decision differs for each: retry with more memory,
retry as-is, do not retry, or drain the worker.

`memory.events` has to be read after `wait4` and before the cgroup is removed —
the only moment the counter is both final and still readable — which is why the
lookup sits inline in `Run` rather than anywhere tidier.

## 2026-09-09 — Every job gets a cgroup, including one with no limits

`Limits{}` writes no interface files, but the cgroup is still created, entered
and removed. `Run` therefore requires cgroups v2 even for an unlimited build.

The alternative — skip the cgroup when no limits are set — adds a branch that
makes every later question ("which cgroup did job X run in?", "what did it
use?") conditional on how it was configured. Accounting that exists only
sometimes is accounting nobody can build on. The cost is one `mkdir` and one
`rmdir` per job, and a requirement the project already had.

## 2026-09-09 — pids.max ships alongside memory and CPU

The Week 3 plan named memory and CPU. `pids.max` is in the first cut too,
because a fork bomb is not an exotic failure in a build sandbox — it is what a
misconfigured `make -j` looks like from the kernel's side, and without a pid cap
it takes the worker down with the build. It costs one line and one number.

Memory and CPU are also the two limits whose *reporting* is interesting: memory
kills and must be distinguished from other kills, CPU throttles silently. pids
just fails the fork, which the build sees directly.

## 2026-09-09 — The cgroup root is a parameter, defaulting to /sys/fs/cgroup/forge

`Spec.CgroupRoot` names the parent that per-job cgroups are created under.

Under systemd the worker is handed a delegated subtree (`Delegate=yes`) and is
expected to stay inside it; systemd reconciles the parts of the tree it owns and
will undo writes elsewhere. Passing the delegated path is then the entire
adaptation, rather than a second code path. Outside systemd — the CI runner,
the colima VM — the default is a cgroup of Forge's own directly under the root.

`prepareRoot` enables `cpu`, `memory` and `pids` in the root's
`cgroup.subtree_control`, and fails with the controller named if the parent did
not delegate it, rather than failing later and less legibly on a missing
`memory.max`.

## 2026-09-09 — /dev is a private tmpfs with six nodes made from their numbers

The sandbox needed a `/dev`. Three ways to get one:

1. **Bind-mount the host's `/dev`.** What most quick fixes do. It hands the
   build `/dev/mem`, `/dev/kmsg` and every block device on the machine. A
   sandbox that can open `/dev/sda` is not a sandbox.
2. **Ship the nodes in the rootfs image.** No mount needed, but the image is
   shared by every build on the worker and is writable, so the nodes are
   shared mutable state — and a build that deletes `/dev/null` breaks the next
   build rather than itself.
3. **A tmpfs mounted inside the sandbox's own mount namespace, with the nodes
   created on it by `mknod`.** Chosen.

The tmpfs makes the device set a property of the code rather than of the host
or the image: exactly the six nodes listed in `devNodes` exist, made from the
major and minor numbers in the kernel's own device registry, and the host's
`/dev` is never read. It also disappears with the mount namespace, so there is
no cleanup path to get wrong.

Two details that are not incidental:

- **The mount is not `MS_NODEV`.** That flag makes device nodes on a filesystem
  inert, and carrying six of them is this filesystem's entire purpose. It is
  `nosuid,noexec`, which costs nothing here.
- **The umask is zeroed around the `mknod` calls.** `mknod` masks its mode the
  way `open` does, so an inherited 022 leaves `/dev/null` at 0644 — which works
  for exactly as long as every build runs as root, i.e. until the user
  namespace lands, and then fails as a permission error a long way from its
  cause.

Not included: `/dev/shm`, `/dev/ptmx` and a `devpts` mount. Nothing needs them
until a build wants a pty or POSIX shared memory, and each is another mount to
justify. `/dev/tty` is included as a node — opening it with no controlling
terminal gives `ENXIO`, which is what a tool probing for a tty expects; its
absence gives `ENOENT`, which some read as "broken system" rather than "no tty".

**This does not narrow the threat model.** A build still runs as real root with
`CAP_MKNOD`, so a hostile one can `mknod` its own `/dev/sda` anywhere it can
write. Bounding that is the user namespace's job, not this change's; what this
change fixes is builds failing by accident.

## Pending decisions

These are called out in the brief and will be written up when the code that
forces them lands: PostgreSQL vs Redis as the queue, leases vs Raft, custom
runtime vs runc/containerd, the Kubernetes privilege model for workers, cache
key composition, and queue-depth vs CPU autoscaling.
