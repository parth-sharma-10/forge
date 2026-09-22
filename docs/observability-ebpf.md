# eBPF syscall observability — design and the decision it waits on

Status: **built.** The dependency decision below was taken —
`github.com/cilium/ebpf`, Forge's second dependency — and the feature shipped in
`internal/syscalls`, behind the worker's `-syscall-metrics` flag (default off).
What was actually built and how it is verified is in the last section, [Built](#built);
the rest of this document is the design it was built from. It is the next
security-observability layer after the seccomp fuzzer and the threat model,
and it was written down before any code because it turned on a dependency
decision that is the owner's, exactly as the queue's PostgreSQL driver was
([roadmap.md](roadmap.md)).

## The goal

Make each job's syscall behaviour observable — a per-job histogram of which
syscalls a build made, and how often — exported through the metrics the worker
already serves. This turns "the filter allows 192 syscalls" from a static claim
into a running measurement: which of those 192 a real build actually uses, and
whether anything is reaching for the edge of the allowlist.

It must not weaken the sandbox. Observation is read-only, runs **outside** the
sandbox in the worker, and never gives the build a new capability, syscall,
device or descriptor.

## Why eBPF, and not something already here

The obvious alternatives were ruled out, each for a concrete reason:

| Approach | Why not |
|---|---|
| `SECCOMP_RET_LOG` on the filter | The filter's denials return `ENOSYS` so a C library falls back to an older call ([decisions.md](decisions.md)). A syscall gets exactly one filter action — you cannot both return `ENOSYS` and log. Logging would mean changing the return, breaking the fallback the sandbox depends on. |
| `SECCOMP_USER_NOTIF` | Adds a supervisor thread in the loop of every syscall it watches, and is a control-plane mechanism (it can allow/deny), not an observer. Wrong tool, and it can become a correctness hazard if the supervisor stalls. |
| `ptrace` / `PTRACE_SECCOMP` | `ptrace` is not on the allowlist and must not be — it is an escape primitive. Tracing from the parent would also serialise the build. |
| `/proc/<pid>/status`, cgroup stats | Give context switches and CPU, not syscall counts. The kernel does not expose per-syscall counters anywhere in procfs or cgroupfs. |
| eBPF `raw_tracepoint/sys_enter`, filtered by cgroup id | The only mechanism that counts syscalls without perturbing the build or weakening the filter. This is what the design uses. |

## The minimal design

- A single `raw_tracepoint/sys_enter` program, loaded once by the **worker**
  (not per job, not by the build).
- The program reads the current task's cgroup id and looks it up in a BPF map
  the worker populates with the cgroup id of each running job; a miss means the
  syscall is not a forge job and is ignored. This is how per-job attribution
  happens without a program per job.
- Counts go into a per-cgroup `BPF_MAP_TYPE_PERCPU_HASH` keyed by
  `(cgroup_id, syscall_nr)`. The worker reads and clears it when the job ends
  and emits the histogram through the existing `internal/metrics` registry
  (`forge_job_syscalls_total{syscall=...}` — a counter family, same text format
  the worker already writes by hand).
- The job's cgroup already exists and its id is knowable from the cgroup fd the
  sandbox opens (`name_to_handle_at`, or the `.id` via `bpf_get_current_cgroup_id`
  matched against a value the worker records at claim time).

That is the whole mechanism: one program, two maps, load at worker start,
enrol a cgroup id per job, drain at job end.

## Graceful degradation — non-negotiable

Observability is optional and must never fail a build or a worker:

- If loading the program fails — no `CAP_BPF`/`CAP_SYS_ADMIN` in the init
  namespace, a kernel without BTF or `raw_tracepoint`, a locked-down host — the
  worker logs one line and runs with observability off. Jobs run identically.
- The feature is behind a `-syscall-metrics` flag, default off, so the common
  deployment carries none of it.
- Kernel floor: `raw_tracepoint` (4.17) and `bpf_get_current_cgroup_id` (4.18)
  put the floor at 4.18; BTF/CO-RE (for a portable program without per-kernel
  builds) wants 5.2+. The worker probes and refuses gracefully below its floor,
  the same way `dropBoundingSet` reads `cap_last_cap` rather than assuming.

## Why it cannot become an escape vector

- The program is loaded by the worker, which is already the privileged side of
  Forge; the build gains nothing. `bpf` stays off the sandbox's seccomp
  allowlist, so a build still cannot load, attach or read a BPF object.
- The program is read-only: `sys_enter` observation increments a counter. It
  writes no task state and calls no helper that can affect the traced process.
- The maps live in the worker's namespace; there is no path from inside the
  sandbox to them.

## The decision this waits on — the owner's

Forge has exactly one dependency (`pgx`), by deliberate choice, and the seccomp
filter is hand-written BPF rather than `libseccomp` for the same reason. eBPF
raises the same question one size larger:

- **`github.com/cilium/ebpf`** (pure Go, no cgo, the standard loader) — the
  pragmatic choice. It would be Forge's **second** dependency. It brings a map/
  program loader, CO-RE relocation and BTF handling that are genuinely hard to
  get right by hand.
- **Hand-rolled `bpf(2)` wrappers + hand-assembled eBPF bytecode** — consistent
  with the hand-written seccomp filter, zero new dependencies, but eBPF is far
  larger than the ~20-instruction classic-BPF seccomp program: a verifier to
  satisfy, maps to create, a program to load and attach, and CO-RE relocations
  for portability. This is days of unsafe, kernel-fragile code.

Recommendation: **`cilium/ebpf`**, accepted as the second dependency, because
the hand-rolled path's cost is out of proportion to an observability feature.
But adding a dependency to Forge is the owner's call, which is why this is a
document and not a pull request.

## Exit criteria (when built)

1. A build's syscall histogram appears in the worker's `/metrics`, per job.
2. On a host without `CAP_BPF`, the worker starts, logs the degradation, and
   runs jobs normally — proven by a test that forces the load to fail.
3. The build still cannot touch `bpf` — the existing seccomp tests already
   prove `bpf` is denied; a test confirms the observability path adds no
   syscall to the allowlist.
4. Attribution is correct under concurrency: two jobs' histograms do not mix —
   proven by a test running two builds with distinct syscall signatures.

## Built

The feature lives in `internal/syscalls`. The worker gains an optional seam,
`sandbox.Spec.Observe`, called with a job's cgroup id after the cgroup is
created and before the build starts, returning a drain that runs after the
build exits and before the cgroup is removed. `cmd/forge-worker` turns it on
with `-syscall-metrics` (default off); a load failure logs one line and the
worker runs jobs unobserved.

Two implementation choices sat under the dependency decision:

- **The program is hand-assembled with cilium/ebpf's `asm` package, not
  compiled from C with clang.** The recommendation above was against
  hand-rolling `bpf(2)` syscall wrappers and bytecode; using cilium/ebpf as the
  loader while writing the program's ~20 instructions in Go keeps the one
  approved dependency, needs no C toolchain in the build, and commits no binary
  object. It is the seccomp filter's reasoning one size up (`program_linux.go`).
- **The metric is `forge_job_syscalls_total{nr="N"}`** — an aggregate counter
  family labelled by syscall number under the build's architecture. A number
  rather than a name because a portable number-to-name table is a maintenance
  burden the number does not need; the label's meaning is the arch's syscall
  table. Per-job attribution is in the mechanism (each job's counts are drained
  from its own cgroup), not in a per-job label, which would be unbounded
  cardinality.

The kernel work: one `raw_tracepoint/sys_enter` program, an `enrolled` hash
(cgroup id → 1) the worker populates per job, and a `counts` per-CPU hash keyed
by `(cgroup id, syscall nr)`. The program reads `bpf_get_current_cgroup_id`,
ignores anything not enrolled, and increments the per-CPU counter — no atomic,
no lock. The cgroup id is the kernfs inode of the job's cgroup directory
(`cgroup.Group.ID`), which since Linux 5.5 is exactly what the helper returns.

Verification, all in CI (`make test-syscalls`, root + a BTF kernel), because
none of it runs on macOS:

1. **Per-job histogram in `/metrics`** — the integration test drains a real
   sandboxed build's counts into the registry and reads them back.
2. **Graceful degradation** — `TestLoadNeverReturnsABrokenObserver` runs
   everywhere: off Linux and on an unprivileged Linux host `Load` returns an
   error and no `Observer`, and the worker's flag path logs and continues.
3. **The build still cannot touch `bpf`** — unchanged: `bpf` is not on the
   seccomp allowlist and the existing filter tests prove it is denied; the
   observability path adds no syscall to the sandbox.
4. **Attribution does not mix** — the integration test runs an openat-heavy
   build and a write-heavy build; each is attributed to its own cgroup, and the
   write-heavy build's openat count stays small while the test process itself
   makes thousands, which is the proof the filter counts the job and not the
   machine.

### What the histogram can and cannot see

Two properties that are easy to assume the wrong way round. Both are pinned by
`TestADeniedSyscallIsNotCounted` on a real kernel.

**A syscall the seccomp filter denies is not counted.** The kernel runs the
filter before the `sys_enter` tracepoint, and a denied call returns without
reaching it. In CI, a job that tried `mount` 201 times (every attempt got
`ENOSYS`) and a job that never tried both counted **exactly 11** mounts. Those
11 came from Forge itself; the attempts were not counted at all. So the
histogram is the *allowed* surface a build actually used. It is not a record
of what the build tried and was refused, and it cannot be used to detect a
build probing the filter. That would need `SECCOMP_RET_TRACE` or a
`seccomp_unotify` listener, and the trade-offs of each are in the table at the
top.

**Forge's own sandbox setup is counted.** The re-executed child builds the
sandbox inside the job's cgroup, before the filter exists: its private
remount, scratch tmpfs, overlay, `/proc`, the `/dev` tmpfs and six device
binds are the 11 mounts above, and `pivot_root` shows up too. Every job
carries this fixed baseline. Subtract a no-op job's histogram to see what the
build alone did.
