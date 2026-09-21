// Package syscalls observes, per job, which syscalls a build makes and how
// often, and exports the histogram through the worker's metrics registry.
//
// It turns the seccomp allowlist from a static claim — "these 192 syscalls are
// permitted" — into a running measurement of which of them real builds reach
// for, and whether any build is pushing at the edge of the allowlist. The
// observation is read-only, runs outside the sandbox in the worker, and gives
// a build no new capability, syscall, device or descriptor: bpf(2) stays off
// the sandbox's allowlist, so a build still cannot load, attach or read any of
// this. The design and the alternatives it was chosen over are in
// docs/observability-ebpf.md.
//
// Only the Linux build does anything. Everywhere else Load returns
// ErrUnsupported and the worker runs jobs with observation off — the same
// graceful degradation as a Linux host without the capability to load a BPF
// program.
package syscalls

import "errors"

// ErrUnsupported is returned by Load where syscall observability cannot run: a
// non-Linux build, a kernel missing raw_tracepoint or bpf_get_current_cgroup_id,
// or a process without the capability to load a BPF program. It is not an error
// the caller must treat as fatal: observation is optional, and a worker that
// cannot load the program logs one line and runs jobs identically.
var ErrUnsupported = errors.New("syscalls: observability unsupported here")

// MetricName is the counter family the drained per-job histograms accumulate
// into. The label is the syscall number under the build's own architecture,
// since a portable number-to-name table is a maintenance burden the number
// itself does not need.
const MetricName = "forge_job_syscalls_total"
