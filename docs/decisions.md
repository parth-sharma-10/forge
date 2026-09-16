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
filesystem would want. Nothing collects them yet.

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

## Pending decisions

These are called out in the brief and will be written up when the code that
forces them lands: PostgreSQL vs Redis as the queue, leases vs Raft, custom
runtime vs runc/containerd, the Kubernetes privilege model for workers, cache
key composition, and queue-depth vs CPU autoscaling.
