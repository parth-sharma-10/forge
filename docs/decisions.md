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

*Amended 2026-09-11:* `/proc` is now mounted before `pivot_root`, not after the
detach, and the reason given here for the old order was wrong — the instance is
the new PID namespace's either way, because that is fixed by the namespace of
the mounting process. The user namespace forced the change and `docs/failures.md`
has the account.

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

*Superseded 2026-09-11* — the mapping landed in Phase 0 Week 5 rather than
Phase 1, after the primitives it changes the meaning of, which is the order
this entry argued for. See below.

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

  *Amended 2026-09-15:* it is `MS_NODEV` now. Once the nodes became bind mounts
  (Week 5), each carried the host mount's flags and the tmpfs's stopped
  reaching them — the mutation harness found the flag had become a no-op, the
  suite passing with it set. Set, it costs nothing and closes the one thing it
  ever governed: a device node created on the tmpfs itself.
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

## 2026-09-11 — The seccomp filter is an allowlist, written as BPF by hand

An allowlist of about 190 syscall numbers, denying everything else, rather than
a blocklist of the dangerous ones.

Rejected: a blocklist, which is what Docker's default profile is. A blocklist
is smaller and easier to read, and it is wrong by construction — every syscall
a future kernel adds is permitted until someone notices it. `openat2` and
`clone3` are both recent enough that a blocklist written three years ago misses
them. The allowlist's cost is real and is the opposite failure: a syscall a
build legitimately needs and nobody listed. That failure is loud, local and
fixed by one line; the blocklist's failure is silent.

Rejected: `libseccomp-golang`. It is the standard tool and it is cgo, which
would put a C toolchain and a shared library between Forge and a static binary,
for a program that is nineteen instructions of encoding. See "Zero dependencies
until one is unavoidable" — this one is avoidable.

Syscall numbers come from Go's own `syscall` package, which resolves them per
architecture at compile time, so the policy is written once as names. Go froze
its Linux tables before `seccomp` and `getrandom` existed on amd64; those two
numbers are written out in `seccomp_linux_amd64.go` and are the only magic
numbers in the package. `golang.org/x/sys/unix` would supply them, and would
be the right answer the moment a third is needed.

## 2026-09-11 — A denied syscall returns ENOSYS, not EPERM

`SECCOMP_RET_ERRNO(ENOSYS)` for everything not on the allowlist.

Why: `ENOSYS` is the one errno a C library treats as "this kernel is older than
I am, use the fallback". glibc issues `clone3` and drops back to `clone` on
`ENOSYS`; it issues `statx` and drops back to `fstatat`. On `EPERM` it does
neither — it propagates the failure. So an allowlist returning `EPERM` breaks
every program built against a libc newer than the list, as an unexplained
failure deep inside a build, and it breaks them *because* the list is an
allowlist. `ENOSYS` makes the allowlist self-healing against exactly the case
it is worst at.

This is also why the allowlist can stay as small as it is: `statx`, `openat2`,
`clone3`, `close_range`, `faccessat2` and `rseq` are all absent, and libc falls
back for each.

Rejected: `EPERM`, which is Docker's default and is right for a boundary whose
denials get audited. Rejected: `SECCOMP_RET_KILL_PROCESS`, which is the only
option that makes a denial unmissable — a build dies with `SIGSYS` and the
kernel logs the syscall number. That is the correct choice for a hostile
workload and the wrong one here, where the filter's first job is to stop
accidents without turning every unlisted syscall into a build failure.

The cost, stated plainly: in a build log a denial is indistinguishable from an
old kernel. Changing it is one constant in `seccomp_linux.go`.

## 2026-09-11 — Sandbox root is host uid 524288, and the map is 65536 wide

`CLONE_NEWUSER` joins the clone flags, with uid and gid 0..65535 inside mapped
to 524288..589823 outside, `setgroups` denied, and `Credential{Uid: 0, Gid: 0}`
moving the child onto the first of them.

Why the credential and not just the map: a child cloned from root keeps the
host root it was cloned with, and the map only describes how ids are named
inside the namespace. Without the `setuid` the process is still uid 0 to the
kernel and the whole change is decoration that passes a test asserting `id -u`
is 0 inside. This is the mutation the suite has to catch, and does.

Dropping from root to another uid normally clears every capability, which would
leave the child unable to mount anything a sandbox needs. It does not here: the
kernel compares against root *of the process's own user namespace*, and 524288
is exactly that, so the kernel reads this as a move to root rather than away
from it. The child keeps `CAP_SYS_ADMIN` in its namespaces and holds nothing on
the host.

Why a range and not one id: a build that `chown`s is the ordinary case — every
package manager does — and a single-id map answers all of them with EINVAL.
65536 ids cost one constant.

Why 524288: past every id a distribution assigns (systemd stops at 65535) and
clear of 100000, where `/etc/subuid` starts handing 65536-id ranges to real
users for rootless podman and docker. A collision is not a hole in the sandbox;
it makes a build's files show up owned by somebody.

Rejected: mapping host uid 0 into the namespace as some high id, which would
keep the rootfs image writable from inside. It would also hand a build a
capability-bearing path to files owned by the real root, which is the thing the
map exists to remove.

Two consequences, both real and neither papered over:

- **The rootfs image is no longer writable from inside.** It belongs to host
  root, which the map does not cover, and `CAP_DAC_OVERRIDE` only reaches files
  owned by a uid the namespace can name. `/tmp` is world-writable in the image
  and is the only part a build can still write — and still the only part one
  build can contaminate for the next. A per-job overlay is what closes that.
- **Every job on a worker shares one map**, so two builds on the same worker
  run as the same host uid. This is a boundary between a build and the host,
  not between two builds. Per-job ranges belong with the overlay.

Two smaller observations from a real run, since both are easy to assume wrong:
the build keeps a full capability set *inside* the namespace
(`CapEff: 000001ffffffffff`) — the `setuid` does not strip it, for the reason
above — and its one supplementary group is the overflow gid, host root's group
seen through a map that does not cover it. `setgroups` being denied is what
stops it trading that for a group that names something.

The `/dev` nodes became bind mounts of the host's in the same change, because a
process in a user namespace cannot create a device node at all — `CAP_MKNOD` is
checked against the initial namespace. The kernel's major and minor numbers stay
in the code as an assertion about the host rather than as arguments to `mknod`.

## 2026-09-16 — Per-job root filesystem is an overlay the child mounts on a tmpfs

Each job runs on an overlay: the image is the lower layer, and the upper and
work directories are on a tmpfs the child mounts in its own mount namespace,
over the image's own path, before `pivot_root`. Nothing is created on the
host; the layer ends with the namespace.

Why the child and not the parent: the parent would have to create a per-job
directory on the host, pass it in, and remove it — and something would have to
sweep the ones a crashed worker leaves. A mount in the job's namespace has no
such path. Why the image path and not a directory of its own: any other mount
point is a host path the sandbox would depend on existing and not containing
the image; the image path is the one directory the sandbox already requires.
The lower layer is named through the working directory so the tmpfs can cover
the path without hiding the image from the mount that needs it.

Rejected: a read-only bind of the image with tmpfs over `/tmp`, `/var/tmp` and
`/root`. Smaller, but a build that installs a package writes to `/usr` and
`/etc` and `/lib/apk`, and a build platform that cannot install build
dependencies is not one. Deferred, not rejected: an upper layer on a host
directory that outlives the job, which is what collecting artifacts from the
filesystem would want. (Artifacts were built on 2026-10-14 without it: the
walk runs inside the namespace before the layer dies.)

**The image is shifted into the uid map.** The kernel will not copy a file up
whose owner is not mapped in the writer's user namespace — `ovl_copy_up_one`
returns `EOVERFLOW` — and host root, which owned every file in the image, is
the one id the map deliberately leaves out. Measured before it was reasoned
about: the first run of the overlay failed `mkdir /tmp/x` with "Value too
large for data type". `hack/shift-rootfs.sh` moves every uid and gid in the
image up by `mappedBase`, once, at unpack; the test suite refuses an image
that has not been shifted and says what to run.

This gives up the Week 5 property that the image belongs to host root and is
unwritable from inside at the permission level. What replaces it is stronger
in the case that matters — the image is never the target of a write at all,
because every write is a copy-up into the layer — and weaker in one that does
not arise: a build that escaped its mount namespace would find the image
owned by its own uid range. The alternative that keeps both, an idmapped bind
mount of the lower layer, has to be made by the parent, against the child's
user namespace, after the child exists, with `open_tree`, `mount_setattr` and
`move_mount` and a hand-off in between. Not for a one-time `chown`.

**No size on the tmpfs.** Its pages are charged to the job's cgroup, so
`MemoryBytes` already bounds the layer; a separate limit would be a second
number to explain that the first one covers. The kernel's default cap of half
of RAM stands for a job with no limit.

**`userxattr`, kept without a test that needs it.** The overlay marks a
directory that hides its lower namesake with an extended attribute, and
upstream `trusted.*` needs `CAP_SYS_ADMIN` in the initial namespace, which
the mounter here does not have; `user.*` is the namespace the option
switches to and the documented choice for an unprivileged mount. Measured:
with the option removed, the suite passes on the colima VM's Ubuntu 6.8 —
including a test that removes and recreates `/etc/apk/keys` and finds it
empty — and `dmesg` shows none of the "failed to set xattr on upper" the
fallback prints. So on that kernel the mounter sets `trusted.overlay.*` from
inside the user namespace, which the upstream permission check does not
allow and Ubuntu's overlayfs patches apparently do. The mutation
`overlay-without-userxattr` is therefore not scored: a harness that fails on
a mutant no kernel it runs on can distinguish is not a gate. The option stays
because it costs nothing and is correct on the kernels that follow upstream.

**Mutation `no-tmpfs-just-nodes` retired.** It dropped the tmpfs under `/dev`
and was caught, in Phase 0, by device-node files leaking into the shared
image. The overlay prevents that leak by itself, so the mutant became
equivalent; per the 2026-09-15 rule it is retired rather than carried as an
expected miss. The tmpfs stays: it still keeps the nodes out of the job's
root filesystem, bounded to 64k and `noexec`.

## 2026-09-16 — clone is refused with EPERM when it asks for a namespace

The filter's one argument check. `clone` must be allowed for a build to fork
at all, and its first argument decides whether the call is a fork, a thread
or a request for new namespaces; the filter loads the low word of it and
refuses the call if any `CLONE_NEW*` bit is set.

`EPERM`, not the `ENOSYS` every other denial returns. `ENOSYS` is a signal to
a C library that the syscall is missing and an older one should be tried;
`clone` is not missing, and there is no older way to create a namespace. The
kernel's own answer to a process that lacks the privilege is `EPERM`, and a
tool that reports it — `unshare: Operation not permitted` — says the true
thing. `clone3` stays denied by number: its flags are in a struct the filter
cannot read, and its documented fallback is `clone`.

Rejected: killing the process instead. A build tool that probes for
namespace support and takes the refusal gracefully is an ordinary thing to
write, and killing it would turn a supported build into a mystery.

## 2026-09-16 — The bounding set is nine capabilities, decided by the tests

After `execve` a build holds chown, dac_override, fowner, fsetid, kill,
setgid, setuid, net_bind_service and setfcap, and nothing else. Everything
above that — `CAP_SYS_ADMIN` first among them — is dropped from the bounding
set before the exec, and `CAP_SETPCAP` last, so the set cannot be widened
again from inside.

The list was chosen by what the existing tests show a build doing: chown to
another uid, chmod, unlink and replace files the image owns, run `apk`. Not
by Docker's default set, which also keeps mknod, sys_chroot, net_raw,
audit_write and setpcap — mknod and chroot the filter denies anyway, and the
other three no build has needed. A capability is added to the list when a
test shows a build needing it, and the test that shows it is the one that
keeps it.

The first behavioural test was wrong, and the mutation harness said so:
`nice -n -5` fails without `CAP_SYS_NICE` — but it failed with the bounding
set intact too, because `can_nice` checks against the initial user
namespace, where this root never had the capability. The mutation
`bounding-set-not-dropped` was caught by the `/proc/self/status` test alone.
A raw socket replaced it: `CAP_NET_RAW` is checked against the namespace
that owns the network namespace, which the sandbox's own user namespace
does. Most capability checks in the kernel are `capable()`, against the
initial namespace, and a test of a dropped capability has to find one of the
`ns_capable()` minority to prove anything.

## 2026-09-16 — The queue is one PostgreSQL table, and pgx is the first dependency

Phase 2's store. Parth chose `jackc/pgx/v5` over `lib/pq` (maintenance
mode) and over SQLite-first (defers the dependency, not the decision, and
writes the claim query twice). It is the only `require` in `go.mod`.

**One table.** A job is a row; a run is an attempt on that row (`attempt`,
`started_at`, `finished_at`, the result columns). The roadmap named three
tables — jobs, runs, leases — and the honest minimum is one: a lease is two
columns on the job, and a history of attempts is something Phase 4 may
want and nothing yet reads.

**`Claim` is one statement** — `UPDATE ... WHERE id = (SELECT ... FOR UPDATE
SKIP LOCKED LIMIT 1) RETURNING` — and it takes an expired lease in the same
`WHERE` as a queued job. Not Phase 4 pulled forward: a lease that nothing
reclaims is a timestamp. There is no sweeper to requeue expired jobs, so
there is no window in which a job is queued but not claimable, and no
sweeper to fail to run.

**A fencing token, not just a lease.** Every claim increments `fence`;
every heartbeat and report names the token it was given and matches no row
if the token has moved on. The worker that lost its lease still holds a
`Job` and can still finish the build; the queue is what refuses the result.
The alternative — trusting the worker to notice the lease expired — trusts
a clock and a process that may have been paused for longer than the lease.

**At-least-once.** A worker that dies after the build and before the report
leaves a job to be run again. Phase 4 measures it. `Report` returning
`ErrLeaseLost` is the visible edge of it today.

**No attempt cap.** Marked `ponytail:` in the schema. Phase 4 owns it.

## 2026-09-16 — The worker is N loops, and the pool is not one of them

`worker.Serve` starts N goroutines, each claiming, running and reporting in
turn. Phase 0 Week 1's `internal/pool` was built as "the concurrency
primitive the worker will use for its job loop", and the loop does not use
it: a pool runs tasks handed to it, and the claim has to happen *inside*
the loop, when a slot is free, or a claimed job waits for a slot while its
lease runs down. The two stops the pool was built for — drain and cancel —
are a channel and a context here, which is what they were in the pool too.
The pool stays for now; if Phase 5 finds nothing using it, it goes.

**The lease is kept by a heartbeat beside the build**, every third of the
lease, and a heartbeat that comes back `ErrLeaseLost` cancels the build:
once another worker holds the job, this result can never be reported, and
the only thing left to do with the build is stop paying for it. The report
goes out on the worker's context, not the build's, so a build cancelled for
a lost lease still tries to report and is told no by the queue.

**The log is a tail**, the last `LogLimit` bytes of stdout and stderr
interleaved, on the job row. The end of a build log is where the reason it
failed is. Streaming is Phase 3.

## 2026-09-16 — Logs ride the heartbeat, and following is polling

The worker already writes the job's row every few seconds to keep its
lease; the log tail goes in the same statement, with the total byte count
beside it. No log table, no second connection, no second goroutine, and a
followed log is at most one heartbeat stale — two seconds, or a third of
the lease if that is shorter. `GET /jobs/{id}/logs` re-reads the row every
half second and writes what is new, by comparing totals. Rejected:
LISTEN/NOTIFY, which cuts the latency nothing needs cut and adds a second
path to keep the first honest.

**Truncation is announced, not hidden.** The row holds a tail; a follower
whose last total is further behind than the tail is long is told how many
bytes it missed. A total that went *down* is another worker's attempt
starting from nothing, and is announced as a restart. Both were chosen over
the silent splice that "just send the suffix" produces.

## 2026-09-16 — Schema application takes an advisory lock

`CREATE TABLE IF NOT EXISTS` is not safe against itself: two sessions that
both find no table both try to create it, and one loses on the catalogue's
unique index. The API and the worker connect at the same moment on every
fresh deployment, and the first time they did, the worker died with exactly
that error. `Open` now applies the schema inside a transaction that holds
`pg_advisory_xact_lock` first. `TestConcurrentOpensOnAFreshDatabase` drops
the table and opens eight at once; with the lock removed it fails three
runs out of three.

The same test found that `Open`'s error path hung: it closed the pool
while its deferred rollback still held the connection, and `pgxpool.Close`
waits for every connection to come back. A schema failure of any kind —
missing grants, say — would have been a daemon that never started and
never said why. The transaction is now released before the pool is closed.

## 2026-09-17 — A dead worker's job runs again; a late worker's report does not land

Phase 4 is the queue's design tested rather than described, and the tests
are the decision record.

**The worker that dies is a real process.** A goroutine cannot be killed,
so `TestAJobSurvivesItsWorker` starts the test binary in a fourth identity
— a worker with a fake runner — waits for it to claim, and `SIGKILL`s it.
The job's lease expires, a second worker reclaims it, and the row says
attempt 2. That is the whole of the at-least-once contract, measured: the
attempt count and the last worker are on the row and in the API.

**The fencing token, discriminated from the state check.** A worker that
stopped heartbeating and lost its lease, then finished its build, reports
while the worker that took the job over is *still running it*. The row is
`running`, which is what the stale report matches on; only the token
refuses it. `TestALateWorkersReportIsRefused` was first written with the
second worker already done, and would have passed on the state check
alone — the mutation `report-without-fence` was what showed it, and the
test now holds the second worker mid-build while the first reports.

**The attempt cap is the worker's, not the query's.** A job past
`MaxAttempts` is claimed like any other and reported as given up without
running. Excluding it in `Claim`'s `WHERE` would leave it `running` with an
expired lease — a row nothing touches again and no state name for what it
is. Three attempts by default; `-max-attempts` on the worker.

**The queue mutations run on the host.** `hack/mutate.py --queue` builds
the queue's tests, runs them against `FORGE_TEST_DSN`, then the worker's;
four mutants — both fence checks, the reclaim clause, the schema lock —
all caught. The harness's first VM mutant that hung a whole test run
(2026-09-11) has a cousin here: a fake runner that ignores its context
turned a failed assertion into a hung `Serve`. Fakes honour `ctx` now.

## 2026-09-17 — Metrics by hand, benchmarks by `go test`, numbers with their machine

**The Prometheus text format is written by hand.** A counter is a name, a
help line, a type line and a number. The client library is for histograms,
summaries and pushes, and Forge has none; the dependency count stays at
one. `internal/metrics` is ninety lines and its tests pin the format. A
value that lives in the database — the queue's depth — is sampled on each
scrape; a sample that fails is absent from the output with a comment
saying why, and the process's own counters are still there. The database
being down must not take the worker's counters with it.

**The benchmarks are `go test -bench`**, in the packages the code lives
in, run the way the tests are run — root, image, database — and skipping
without them. `BenchmarkRun` is the sandbox's whole setup and teardown
around an exec of `/bin/true`; `BenchmarkThroughput` is jobs per second
through the queue with four loops. The numbers are quoted with the
machine, because a number without one is a claim.

**What the throughput benchmark measured first was PostgreSQL.** Runs
swung between 25 ms and 229 ms per job, single loop or four, with no
change to the code. The table was carrying every earlier run's rows, each
updated several times and never vacuumed; emptied, the benchmark settled
at 10–18 ms. The benchmark empties the table now, and the deployment
document says what to do about it in production — lower autovacuum's
thresholds, and archive done rows. The sandbox itself, `BenchmarkRun`,
reads 11–39 ms on the development VM, most of that the mounts, and the
range is the VM's, not the code's: six runs of a hundred, no change
between them but the host.

**systemd, not a container, and the units are verified.** The sandbox
needs a kernel and a delegated cgroup subtree; a container runtime is the
thing Forge exists not to need. `deploy/*.service` pass `systemd-analyze
verify` in CI with the binaries installed where the units say.

## Pending decisions

These are called out in the brief and will be written up when the code that
forces them lands: PostgreSQL vs Redis as the queue, leases vs Raft, custom
runtime vs runc/containerd, the Kubernetes privilege model for workers, cache
key composition, and queue-depth vs CPU autoscaling.

## 2026-09-20 — The seccomp filter is fuzzed, and the interpreter is grounded

**The fuzzer is Go's native `testing.F`, not a harness of our own.** The
input space is `(arch, nr, arg0)` and the property is that the compiled BPF
filter agrees with an independent statement of the policy. The stdlib
generates, mutates, shrinks and records escapes; adding a generator would be
code to maintain for what `go test -fuzz` already does. The seed corpus runs
in the ordinary suite, so the boundaries are checked on every run and every
architecture, and `make fuzz` explores outward. See
[seccomp-fuzzer.md](seccomp-fuzzer.md).

**The oracle is written from the policy, not the encoding.** `policyResult`
decides from `allowedSyscalls` and the clone rule, with none of the filter's
jumps and masks. The two share the policy — that is the single source of
truth — and differ in how they express it, which is exactly the jump-overflow
and fall-through class the filter's own comment warns about.

**`evaluate` is grounded against the kernel once, on purpose.** Every seccomp
test trusts the pure-Go interpreter to predict the kernel; nothing proved it
did. `TestInstalledFilterMatchesTheModel` runs the production filter for real
inside the sandbox and compares the kernel's errno to the model's prediction.
It is deliberately a curated, safe set — every syscall in it is refused before
the kernel executes it — rather than a second, live-fire fuzzer, which would
be a way to corrupt the process, not to test a filter.

## 2026-09-21 — eBPF is the second dependency, and the program is hand-assembled

**`github.com/cilium/ebpf` is Forge's second dependency, after `pgx`.** Per-job
syscall observability needs a loader that satisfies the verifier, creates and
reads maps, and attaches a program; hand-rolling `bpf(2)` wrappers and CO-RE is
days of kernel-fragile code for an observability feature. The dependency was
the owner's call, held as a document ([observability-ebpf.md](observability-ebpf.md))
until taken.

Rejected: hand-rolled `bpf(2)`, for the reason above.

**But the program itself is hand-assembled in cilium/ebpf's `asm` package, not
compiled from C.** The dependency buys the loader; the program is ~20
instructions — one helper call, two map lookups, an increment — small enough
that writing it in Go keeps the build free of a C toolchain and commits no
binary object, while still getting cilium's verifier front-end. Same reasoning
as the hand-written seccomp filter, one size up.

Rejected: `clang` + `bpf2go` with an embedded object. It is the standard
pattern and right for a large program, but here it would add a build-time
toolchain and a committed `.o` for twenty instructions.

**The metric is labelled by syscall number, not name.** A portable
number-to-name table is a maintenance burden the number itself does not need;
the label's meaning is the build architecture's syscall table. Revisit if an
operator asks for names in the exposition rather than at read time.

The feature is behind `-syscall-metrics` (default off) and degrades to a
logged line on any host that cannot load it, so the common deployment carries
none of it.

## 2026-09-22 — API auth is a static bearer token, default-deny

The API authenticates every route with one shared bearer token, compared in
constant time, and `forge-api` refuses to start without one unless
`-insecure-no-auth` is passed. Forge targets trusted/internal workloads: the
token gates who may submit builds, and that is the whole identity model.

Rejected, for now: mTLS and per-user tokens with roles. mTLS is the stronger
boundary and the likely next step for a networked deployment, but it moves the
hard part (certificate issuance and rotation) outside Forge, and a shared token
behind a TLS-terminating proxy is the shape most internal deployments actually
run. Per-user identity has nothing to authorize yet — there is no per-user
resource to scope — so it would be a model without a use. Default-deny is the
part that is not negotiable: an execution API must not come up open by accident.

## 2026-09-22 — Per-job uid isolation waits on idmapped mounts

Every job on a worker maps to the same host uid range (`524288..589823`). Real
per-job ranges are deferred, and deliberately not faked with a per-job offset.

Why it is not a config knob: the shared image is pre-shifted to this one range
by `hack/shift-rootfs.sh` so the overlay can copy files up. Give a job a
different range and the image is owned by ids it cannot read, and copy-up
breaks. The correct fix is an **idmapped mount** of the read-only lower layer
per job, so one on-disk image presents different ownership to each job without
a second copy. That is a real change to the mount path, with its own tests and
kernel-version floor (5.12+), and it is worth doing as its own change rather
than bolted onto a hardening pass. Until then the shared range is safe only
because the overlay gives no two jobs a shared writable filesystem — documented
in the threat model, not implied away.

## 2026-09-22 — Cancellation rides the heartbeat

**`POST /jobs/{id}/cancel` sets a flag on the row; the worker learns of it
from its next heartbeat.** A queued job is finished on the spot, in the same
statement, and no worker ever sees it. A running one stays `running` until
its worker's heartbeat returns `ErrCancelled`. The worker then kills the build
exactly as a timeout would and reports it, with a fence that is still valid.
The latency is one heartbeat interval: `min(lease/3, 2s)`.

Rejected: a push channel to the worker (`LISTEN/NOTIFY`, or a worker-side
listener). It would be faster, but it is a second path that can fail
separately from the lease. The heartbeat already carries the log up and the
lease-lost signal down, and cancellation is one more bit on that round trip.

Two edges are handled in code and each has its own test. A job cancelled while
its worker was dead is reclaimed with `Job.Cancelled` set and is reported
without running. A build that exits on its own in the same interval as the
cancel keeps its own exit status: only a build the cancel actually killed is
reported `cancelled`. A finished job cannot be cancelled (`409`), and its
result stands.

## 2026-10-14 — Artifacts come out through a supervisor PID 1, as a tar over a pipe

The owner chose option B of [artifacts.md](artifacts.md). When a job collects
artifacts, the child no longer execs the build over itself. It stays as PID 1,
forks the build, reaps everything until the build exits, kills and reaps what
is left, then writes a tar of `/out` to a pipe. The worker stores the tar in
`jobs.artifact` and the API serves it.

**Why the walk is inside the sandbox.** Walking an upper layer on the host
would mean root code on the host handling an attacker-built tree: symlinks,
FIFOs, whiteouts, sparse files, crash cleanup. Inside, the walk runs under
the namespaces, the filter and the build's own capability set, and nothing is
created on the host. Killing everything first removes every race between a
check and an open.

**Why the supervisor cuts its own capabilities.** The bounding set limits what
an exec confers, not what the process holds. Without the `capset`, the
supervisor would read the build's tree holding every capability of its user
namespace. A non-dumpable flag keeps the build from ptracing it.

**Why one byte before the archive.** PID 1 of a PID namespace cannot die of a
signal it sends itself, and the build's signal has to reach the worker as a
kill, not as an exit status a shell would print as 139. So the supervisor
reports the signal and waits, and the parent SIGKILLs it. The alternative,
exit codes of 128+n, would turn every OOM kill and segfault into an ordinary
failure in the worker's accounting.

**Rejected: per-job opt-in.** A field on `POST /jobs` and a column to carry
it. The flag is per worker instead (`-artifact-mb`, default 16; 0 is the old
sandbox exactly), because nothing yet needs two kinds of job on one worker.

**Rejected: following the design's "the build is no longer PID 1" for every
job.** The supervisor adds 4.4 MB resident and a fork to each job and puts a
Go runtime's syscalls into the job's histogram. A worker that collects
nothing should not pay for it.

## 2026-09-22 — A heartbeat writes the log only when it has grown

**Measured first.** A heartbeat that re-sent an unchanged, incompressible
1 MiB tail wrote **1,152,931 bytes of WAL**. Postgres re-TOASTs a bytea
parameter even when it matches the stored value. A quiet build that has filled
its tail heartbeats every two seconds, so that came to about 0.55 MB/s of WAL
per job for no new output. After the change: **176 bytes** per heartbeat
(`TestAnUnchangedLogIsNotRewritten` logs the number).

**The rule.** The heartbeat keeps the stored log when `log_bytes` equals the
count it is sent, so Postgres reuses the existing TOAST pointer. The worker
also leaves the tail off in that case, which saves the network transfer. This
depends on an invariant: within one attempt the output is one append-only
stream, so an equal count means equal bytes. To keep that true across
attempts, `Claim` now resets the log to empty. Otherwise a new attempt whose
output happened to be exactly as long as the dead one's would never replace
it (`TestAReclaimedJobDoesNotKeepTheDeadAttemptsLog`). The side effect is that
a reclaimed job's old output disappears at the claim rather than at the new
attempt's first heartbeat, which the follower already reports as a restart.

Rejected: append-only log chunks in their own table. That removes the rewrite
of a *growing* tail as well, but it adds a table, a join and a retention path.
Revisit if a chatty build's rewrite of a changing tail shows up in a
measurement.
