// Package cgroup applies cgroups v2 resource limits to a job.
//
// A Group is one cgroup directory holding exactly one job. It is created
// before the job starts and removed after it exits, and the job is placed in it
// by the kernel at clone time rather than by writing a PID afterwards — see
// Create.
//
// Only the Linux build does anything. The limit values themselves are portable
// so they can be constructed, validated and tested anywhere.
package cgroup

import (
	"fmt"
	"strconv"
)

// DefaultRoot is the parent cgroup that per-job cgroups are created under.
//
// It is a parameter rather than a constant used inline because under systemd
// the worker is handed a delegated subtree (Delegate=yes) and must confine
// itself to it; writing to /sys/fs/cgroup directly would be fought over by
// systemd, which reconciles the tree it owns. Passing the delegated path here
// is the whole adaptation needed.
const DefaultRoot = "/sys/fs/cgroup/forge"

// cpuPeriod is the accounting window for cpu.max, in microseconds. 100ms is
// the kernel's own default; a job's quota is expressed against it.
const cpuPeriod = 100_000

// Limits are the resource caps for one job. A zero field means unlimited, and
// writes no file at all — a fresh cgroup is already unlimited, so the absence
// of a limit and the string "max" are the same thing to the kernel.
type Limits struct {
	// MemoryBytes caps anonymous memory and page cache together (memory.max).
	// Exceeding it makes the kernel reclaim first and then OOM-kill inside the
	// cgroup, which Group.OOMKilled reports.
	MemoryBytes int64

	// CPUCores is a fraction of one core's time per period (cpu.max). 0.5
	// means half a core; 2 means two cores' worth. Exceeding it throttles,
	// it does not kill.
	CPUCores float64

	// MaxPIDs caps the number of processes (pids.max). It is what makes a
	// fork bomb in a build fail rather than take the worker down with it.
	MaxPIDs int
}

// Validate rejects limits the kernel would reject less legibly.
func (l Limits) Validate() error {
	if l.MemoryBytes < 0 {
		return fmt.Errorf("cgroup: memory limit %d is negative", l.MemoryBytes)
	}
	if l.CPUCores < 0 {
		return fmt.Errorf("cgroup: cpu limit %v is negative", l.CPUCores)
	}
	if l.MaxPIDs < 0 {
		return fmt.Errorf("cgroup: pid limit %d is negative", l.MaxPIDs)
	}
	// The kernel's floor for cpu.max quota is 1000us. Rounding a smaller
	// request up to it silently would hand back a limit nobody asked for.
	if q := l.cpuQuota(); l.CPUCores > 0 && q < 1000 {
		return fmt.Errorf("cgroup: cpu limit %v is below the kernel's minimum quota (%dus of %dus)", l.CPUCores, q, cpuPeriod)
	}
	return nil
}

func (l Limits) cpuQuota() int64 { return int64(l.CPUCores * cpuPeriod) }

// file is one cgroup interface file and the value to write to it.
type file struct{ name, value string }

// files returns the writes that express l, in the order they should be applied.
func (l Limits) files() []file {
	var fs []file
	if l.MemoryBytes > 0 {
		fs = append(fs,
			file{"memory.max", strconv.FormatInt(l.MemoryBytes, 10)},
			// A memory limit a job can dodge by swapping is not a limit, and
			// swap makes the OOM point depend on the host's free swap rather
			// than on the number configured here.
			file{"memory.swap.max", "0"},
		)
	}
	if l.CPUCores > 0 {
		fs = append(fs, file{"cpu.max", fmt.Sprintf("%d %d", l.cpuQuota(), cpuPeriod)})
	}
	if l.MaxPIDs > 0 {
		fs = append(fs, file{"pids.max", strconv.Itoa(l.MaxPIDs)})
	}
	return fs
}
