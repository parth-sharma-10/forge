# Threat model

What Forge defends against, what enforces each boundary, and — the half that
matters more — what it does not defend against. Nothing here is aspirational:
every control names the code that implements it and the test that proves it,
and everything not implemented is in *What Forge does not defend against*
rather than implied away.

## The adversary

**The build command is hostile and arbitrary.** It is attacker-supplied code
running as root inside the sandbox, and it may be written specifically against
Forge with full knowledge of this document. It can issue any syscall, fork,
thread, write anywhere it can reach, and try to stay alive past its job.

**The image is not hostile.** The root filesystem is supplied by the operator,
unpacked once by `make rootfs`, and shared as the overlay's read-only lower
layer by every job on the host. A build cannot modify it (that is deliberate
and tested), but a malicious *image* is outside the model — it would be the
operator attacking themselves.

**A caller holding the API token is trusted to submit builds.** Every route
wants `Authorization: Bearer <token>`, compared in constant time, and
`forge-api` will not start without a token unless the operator passes
`-insecure-no-auth` for a proxy that authenticates in front
(`internal/api/api.go`, `cmd/forge-api`). A caller *without* the token is
refused; a caller with it can run any build, which is the point of the API. The
adversary modelled below is the build itself, and it starts *after* a job is
accepted.

### Assets, in the order losing them hurts

1. The host kernel and root — a build must not become host root or run code
   outside its namespaces.
2. Other jobs — one build must not read, write or influence another.
3. The host filesystem, including the shared image.
4. The queue's integrity — a result must belong to the run that produced it.
5. Host availability — one build must not starve the others.

## Boundaries and what holds them

| # | The build must not | Mechanism | Code | Proven by |
|---|---|---|---|---|
| 1 | Be a privileged host user | User namespace: uid/gid 0..65535 inside → 524288..589823 outside, `Credential` moves the child onto the first | `sandbox_linux.go` | `TestSandboxRootIsAnUnprivilegedHostUid` |
| 2 | Reach the host filesystem | Mount namespace, `MS_REC\|MS_PRIVATE`, bind + `pivot_root` + `umount2(MNT_DETACH)` | `sandbox_linux.go` | `TestHostFilesystemIsUnreachable`, `TestMountsDoNotLeakToHost` |
| 3 | Write the shared image, or leave state behind | Per-job overlay on a tmpfs in the job's own mount namespace | `sandbox_linux.go` | `TestTheImageIsNeverWritten`, `TestNoWritableStateOutlivesAJob` |
| 4 | See or signal host processes | PID namespace; the build is PID 1 of its own, or its child when artifacts are collected | `sandbox_linux.go` | `TestCommandRunsAsPIDOne`, `TestProcIsTheSandboxProcessTable` |
| 5 | Reach the network | Network namespace with no interfaces at all | `sandbox_linux.go` | `TestNetworkNamespaceHasNoHostInterfaces` |
| 6 | Share memory or message queues | IPC namespace | `sandbox_linux.go` | `TestEveryNamespaceIsNew` |
| 7 | Reach the kernel-facing syscall surface | Deny-by-default seccomp allowlist (192 syscalls on amd64, 159 on arm64); `mount`, `pivot_root`, `ptrace`, `bpf`, `init_module`, `keyctl`, `perf_event_open`, `unshare` and the rest are absent from it | `seccomp_linux.go` | `TestFilterDeniesTheKernelFacingSurface`, `TestSeccompDeniesMount`, `TestSeccompDeniesMknod`, **`FuzzFilterMatchesPolicy`**, **`TestInstalledFilterMatchesTheModel`** |
| 8 | Create namespaces of its own | `clone` filtered on the low word of `args[0]`: any `CLONE_NEW*` bit → `EPERM`; `clone3` denied by number | `seccomp_linux.go` | `TestFilterDeniesCloneWithNamespaceFlags`, `TestABuildCannotCreateANamespace` |
| 9 | Hold a kernel-facing capability after `execve` | Capability bounding set cut to nine; `CAP_SETPCAP` dropped last and not kept | `sandbox_linux.go` | `TestTheBoundingSetIsTheKeepList`, `TestADroppedCapabilityIsGone` |
| 10 | Make a device node | Filter answers `mknod`/`mknodat` `ENOSYS`; the user namespace would refuse it anyway | `seccomp_linux.go` | `TestSeccompDeniesMknod`, `TestDevIsOnlyTheMinimalSet` |
| 11 | Escape its resource limits | cgroups v2 per job, entered at clone time via `CLONE_INTO_CGROUP` — no uncapped window | `internal/cgroup` | `TestSandboxRunsInsideItsOwnCgroup`, `TestMemoryLimitIsReportedAsAnOOMKill` |
| 12 | Outlive its job | Context cancel kills PID 1 and the kernel reaps the namespace; `Pdeathsig=SIGKILL` covers a dead worker | `sandbox_linux.go` | `TestContextCancelKillsTheSandbox`, `TestCgroupIsRemovedAfterTheJob` |
| 13 | Have a stale result accepted | Fencing token per claim; `Heartbeat`/`Report` match only the live token | `internal/queue` | `TestAStaleFenceCannotReportOrHeartbeat`, `TestALateWorkersReportIsRefused`, `make mutate-queue` |
| 14 | Inherit a host file descriptor across `execve` | Go opens its descriptors `O_CLOEXEC`; the raw `syscall.Exec` closes them, leaving only stdio | `sandbox_linux.go` | `TestNoHostDescriptorLeaksIntoTheBuild` (the CVE-2024-21626 class) |
| 15 | Read the host's environment | The build is exec'd with a fixed two-variable environment (`PATH`, `HOME`); the worker's environment — where a registry token or DB password lives — is never forwarded | `sandbox_linux.go` | `TestTheBuildGetsACleanEnvironment` |
| 16 | Learn where it sits in the host's cgroup tree | Cgroup namespace (`CLONE_NEWCGROUP`), created in the same clone that enters the job's cgroup, so its root is the job's cgroup | `sandbox_linux.go` | `TestSandboxRunsInsideItsOwnCgroup` (`/proc/self/cgroup` is `0::/`) |
| 17 | Fill the disk, or exhaust inodes | The overlay's tmpfs is mounted with `size=` and `nr_inodes=` from `Spec.ScratchBytes` (`-disk-mb`): a fill gets `ENOSPC`, not an OOM kill | `sandbox_linux.go` | `TestTheDiskLimitStopsAHostileFill`, `TestTheInodeLimitStopsFileExhaustion` |
| 18 | Hold a worker slot forever | Wall-clock deadline on the build's context (`-timeout`, 1h by default); expiry kills PID 1 and reports `timeout` | `internal/worker` | `TestAJobIsKilledWhenItTimesOut` |
| 19 | Make artifact collection read outside `/out`, block, or exceed its cap | The walk runs inside the sandbox, in the supervisor, after `kill(-1, SIGKILL)` and a reap have emptied the PID namespace. Entries are `lstat`'d and symlinks are recorded, never followed. Only regular files are opened (`O_NOFOLLOW\|O_NONBLOCK`). The archive stops before the entry that would cross the cap, and the worker reads no more than the cap. | `archive.go`, `supervise_linux.go` | `TestASymlinkInOutIsRecordedNotFollowed`, `TestASocketInOutIsSkippedWithoutBlocking`, `TestArtifactsOverTheCapAreTruncatedAndReported`, `TestABackgroundProcessIsDeadBeforeTheWalk`, `TestArchive*` |
| 20 | Take over the supervisor that collects its artifacts | Before the filter goes on, the supervisor cuts its capability sets to the build's keep list and makes itself non-dumpable, so the build cannot ptrace it or read its memory. The artifact pipe is close-on-exec. | `supervise_linux.go` | `TestTheSupervisorIsNoStrongerThanTheBuild`, `TestTheArtifactPipeDoesNotReachTheBuild` |

Two properties fall out of the shape rather than being coded for: a build
cannot outlive its context by forking away, because killing PID 1 of a PID
namespace makes the kernel kill the rest; and the user namespace is what gives
the other five their teeth, since it owns them, so `CAP_SYS_ADMIN` inside is
nothing on the host side of the same boundary.

## The seccomp filter, specifically

It is the boundary most likely to be attacked and the one most recently
strengthened, so it is worth stating what is and is not proven about it.

**Proven.** The filter is an allowlist of syscall *numbers* plus one argument
check on `clone`. The compiled BPF now agrees with an independently written
statement of that policy across the whole `(arch, nr, arg0)` input space —
`FuzzFilterMatchesPolicy`, ~1.6M executions with no escape — rather than only
on the syscalls a point test named. The pure-Go interpreter that every other
seccomp test trusts is itself checked against the real kernel's errno by
`TestInstalledFilterMatchesTheModel`. The architecture is compared before the
number, so a syscall issued under the x32 or i386 ABI — where the same numbers
name different calls — is killed rather than matched against the wrong table.
See [seccomp-fuzzer.md](seccomp-fuzzer.md).

**Not proven, and not provable this way.** That 192 allowed syscalls contain
no exploitable kernel bug. An allowlist bounds the *reachable* surface; it says
nothing about the correctness of what remains reachable. `ioctl` is on the list
and is a large, device-dependent surface in itself; `futex`, `mmap` and
`openat` have all carried privilege-escalation CVEs. The filter makes the
surface smaller and auditable, which is the most a syscall filter can do.

**A denial is quiet by design.** Unlisted syscalls return `ENOSYS`, because
that is the one errno a C library treats as "fall back to an older call". The
cost is that a denial reads like an old kernel in a build log rather than like
an attack. Forge does not currently distinguish the two, and there is no
auditing of denials — `SECCOMP_RET_KILL_PROCESS` or a `seccomp_unotify`
listener is a one-constant change in `denyAction` for an operator who wants
denials loud, and that trade is recorded in `seccomp_linux.go`.
With `-syscall-metrics`, denials are visible after all, per job and by syscall
number, as `forge_job_syscalls_enosys_total`. The exit tracepoint fires for a
call the filter skipped, carrying its ENOSYS. The main histogram cannot show
this: the filter runs before the enter tracepoint that histogram counts from,
and 201 denied `mount` calls added nothing to it
([observability-ebpf.md](observability-ebpf.md)). The ENOSYS count is an
observation, not an alarm: nothing acts on it.

## What Forge does not defend against

Stated plainly because a threat model that lists only its wins is marketing.

**Kernel exploits.** The single most likely real escape. A build reaching a
bug in any of the 192 allowed syscalls can take the host kernel, and every
control above is downstream of the kernel being sound. Forge runs untrusted
code on a shared kernel; that is the architectural bet, and a hypervisor
boundary (Firecracker, Kata, gVisor's userspace kernel) is what buys out of it.
Keep the host kernel patched; this is not a sandbox that makes an unpatched
kernel safe.

**Hardware side channels.** Spectre, Meltdown, L1TF, MDS and their successors.
Forge sets no CPU pinning, no core scheduling, no `spectre_v2` mitigations of
its own, and does nothing about a build measuring another tenant through
shared cache. Anything that must not co-reside should not co-reside.

**Resource exhaustion outside the capped dimensions.** Memory, CPU, PIDs,
disk, inodes and wall-clock time are capped per job — the last three only when
the operator sets `-disk-mb` and keeps `-timeout` non-zero. Nothing caps file
descriptors beyond the rlimit the build inherits from the worker, dirty page
writeback pressure, or the kernel memory a build can pin through legitimate
allocations. Without `-disk-mb` the upper layer has the kernel's default tmpfs
cap of half of RAM, and a job's memory limit is what stops it first.

**A build can end its own job early by signalling its supervisor.** When
artifacts are collected, PID 1 is a Go process, and the Go runtime installs
handlers for `SIGTERM`, `SIGQUIT` and the rest, so a build that signals PID 1
can make the supervisor exit and take the sandbox with it. That ends only its
own job, which the build could do by exiting. It cannot change what the
supervisor reads or reports for a build that has already exited, because by
then nothing of the build is running.

**An artifact is hostile to whoever extracts it.** The tar holds what the build
left in `/out`: symlinks that point anywhere are recorded as symlinks.
`GET /jobs/{id}/artifact` serves it as an attachment, and a consumer that
extracts it must refuse absolute names, `..`, and links out of the extraction
root. That boundary is the consumer's, not Forge's.

**Jobs share a uid range.** Every job on a worker maps to the same host uids.
Since the overlay no two jobs share a writable filesystem, so what remains is
on the host's side: anything a job created outside its layer — nothing today —
would belong to the same ids as every other job's. Per-job uid ranges are the
fix if a later phase gives builds a way to write outside the layer.

**The API's authentication is one shared secret.** A static bearer token, not
users or roles: anyone who holds it can submit, read and follow every job, and
there is no revocation short of changing it and restarting `forge-api`. It
travels in a header over plain HTTP, so off loopback it wants TLS in front.
`-insecure-no-auth` turns the check off entirely, for a proxy that
authenticates; run that way without one, reaching the listener is running a
build.

**At-least-once delivery.** A worker that dies between the build and the
report leaves a job whose lease expires and which runs again. The attempt
count on the row says it happened. Nothing makes the second run harmless —
idempotency is the caller's business.

**Artifacts and covert channels out of a job.** Nothing a build writes
survives it, so the only sanctioned output is stdout, which the worker
captures. Exit status and timing are unmeasured channels; a build can signal
through how long it takes and what it prints. Forge does not model
confidentiality between a job and its own submitter.

**Denial of service by a hostile image.** Out of scope per *The adversary*.

## Known escape classes, and where Forge stands

Container escapes cluster into a few shapes. This is where Forge sits against
each — claims here are design arguments, and the ones marked *unverified* have
not been reproduced against a running Forge.

| Class | Example | Forge's position |
|---|---|---|
| Leaked host file descriptor the runtime holds across the boundary | `CVE-2024-21626` (runc, `/proc/self/fd` working directory) | **Verified by test.** The child execs the build with `syscall.Exec`, a raw execve that closes only `O_CLOEXEC` descriptors, so this is a real vector, not a theoretical one. `TestNoHostDescriptorLeaksIntoTheBuild` enumerates the build's `/proc/self/fd` and asserts it inherits only stdio and no filesystem descriptor; injecting a deliberate leak makes it fail, so the pass is meaningful. Go opens its own descriptors `O_CLOEXEC`, and the cgroup fd is the parent's, used by `clone3` and never in the child — the test is what proves the result rather than the argument. |
| Writable host path bind-mounted in | mounted docker socket, `/proc` or `/sys` from the host | Nothing of the host is mounted except six `/dev` character nodes, each bind-mounted after its major:minor is asserted. No `/sys`, no socket. |
| `mknod` a block device for the host disk | classic privileged-container escape | Denied twice: the filter answers `mknod` `ENOSYS`, and a user namespace cannot create device nodes regardless. |
| Abusing `CAP_SYS_ADMIN` to mount or `pivot_root` back out | privileged containers | `CAP_SYS_ADMIN` is not in the bounding set after `execve`, and `mount`/`pivot_root`/`chroot` are not on the allowlist. |
| Creating a nested namespace to reset the boundary | `unshare`/`clone(CLONE_NEWUSER)` | `unshare` is not on the allowlist; `clone` with any `CLONE_NEW*` bit is `EPERM`; `clone3` is denied by number. |
| Escaping via the cgroup filesystem | `release_agent` write | No `/sys` is mounted, the build is not host root, and the cgroup namespace roots any cgroupfs it could see at its own cgroup. |
| Kernel LPE through an allowed syscall | many | **Not defended.** See above. |

## Review triggers

This document is wrong the moment any of these changes, and each should bring
it back for review: a syscall added to the allowlist; a capability added to the
bounding set; anything new mounted into the sandbox (`/sys`, `/dev/ptmx`,
`/dev/shm`); any change to how artifacts are read (the walk in the supervisor)
or where they go (one `bytea` column); a network interface; and any change to
the uid map or to who the worker runs as.
