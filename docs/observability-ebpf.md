# eBPF syscall observability — design and the decision it waits on

Status: **designed, not built.** This is the next security-observability layer
after the seccomp fuzzer and the threat model. It is written down before any
code because it turns on a dependency decision that is the owner's, exactly as
the queue's PostgreSQL driver was ([roadmap.md](roadmap.md)).

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
