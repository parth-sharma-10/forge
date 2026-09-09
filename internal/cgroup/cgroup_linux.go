//go:build linux

package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// controllers are enabled on the root's subtree so that per-job cgroups
// underneath can carry these limits. A controller can only be enabled for
// children if the parent was itself granted it.
var controllers = []string{"cpu", "memory", "pids"}

// Group is one cgroup directory containing exactly one job.
type Group struct {
	path string
	dir  *os.File
}

// Create makes a new empty cgroup under root with the given limits applied,
// creating and configuring root itself if needed.
//
// The returned Group holds an open file descriptor to the directory, for
// SysProcAttr.CgroupFD. That is the point of the design: the kernel places the
// process in the cgroup as part of clone(2), so there is no window between
// fork and exec in which an untrusted command runs without its limits. Writing
// a PID into cgroup.procs after starting the process leaves exactly that
// window.
//
// The caller must Close the Group to remove the cgroup.
func Create(root string, limits Limits) (*Group, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if root == "" {
		root = DefaultRoot
	}
	if err := prepareRoot(root); err != nil {
		return nil, err
	}

	// MkdirTemp for the uniqueness: concurrent jobs in one worker must not
	// collide, and the kernel has no opinion about the name.
	path, err := os.MkdirTemp(root, "job-")
	if err != nil {
		return nil, fmt.Errorf("cgroup: create job cgroup: %w", err)
	}

	g := &Group{path: path}
	for _, f := range limits.files() {
		if err := os.WriteFile(filepath.Join(path, f.name), []byte(f.value), 0o644); err != nil {
			g.remove()
			return nil, fmt.Errorf("cgroup: set %s=%s: %w", f.name, f.value, err)
		}
	}

	dir, err := os.Open(path)
	if err != nil {
		g.remove()
		return nil, fmt.Errorf("cgroup: %w", err)
	}
	g.dir = dir
	return g, nil
}

// prepareRoot creates the parent cgroup and enables the controllers that job
// cgroups underneath need.
func prepareRoot(root string) (err error) {
	_, statErr := os.Stat(root)
	preexisting := statErr == nil
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("cgroup: create root %s (is cgroups v2 mounted?): %w", root, err)
	}
	// A root we created and then could not configure is a cgroup nobody will
	// ever use and nobody will ever remove — and its presence makes its own
	// parent un-removable. One we were handed is not ours to delete.
	defer func() {
		if err != nil && !preexisting {
			os.Remove(root)
		}
	}()

	available, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("cgroup: read controllers of %s: %w", root, err)
	}
	granted := strings.Fields(string(available))
	for _, c := range controllers {
		if !slices.Contains(granted, c) {
			return fmt.Errorf("cgroup: controller %q is not delegated to %s (it has %v); enable it in the parent's cgroup.subtree_control", c, root, granted)
		}
	}
	// Writing a controller that is already enabled is a no-op, so this is
	// idempotent across every job.
	enable := "+" + strings.Join(controllers, " +")
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte(enable), 0o644); err != nil {
		return fmt.Errorf("cgroup: enable %s on %s: %w", enable, root, err)
	}
	return nil
}

// Path is the cgroup's directory.
func (g *Group) Path() string { return g.path }

// FD is the directory file descriptor for SysProcAttr.CgroupFD.
func (g *Group) FD() int { return int(g.dir.Fd()) }

// OOMKilled reports whether the kernel killed anything in this cgroup for
// exceeding its memory limit.
//
// It exists because the exit status cannot answer the question: an OOM kill
// arrives as SIGKILL, indistinguishable from a timeout, an operator's `kill
// -9`, or the job killing itself. The scheduler needs to tell "this build
// needs more memory" from "something killed this worker".
func (g *Group) OOMKilled() (bool, error) {
	events, err := os.ReadFile(filepath.Join(g.path, "memory.events"))
	if err != nil {
		return false, fmt.Errorf("cgroup: read memory.events: %w", err)
	}
	for _, line := range strings.Split(string(events), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok || key != "oom_kill" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return false, fmt.Errorf("cgroup: parse memory.events oom_kill %q: %w", value, err)
		}
		return n > 0, nil
	}
	return false, nil
}

// Close releases the descriptor and removes the cgroup.
func (g *Group) Close() error {
	if g.dir != nil {
		g.dir.Close()
		g.dir = nil
	}
	return g.remove()
}

// remove deletes the cgroup directory, waiting briefly for the kernel to
// finish reaping its processes.
//
// rmdir on a cgroup that still has members fails with EBUSY, and the last
// task's exit is not synchronous with the parent's wait4 returning. Retrying is
// not politeness: a cgroup that is never removed is a kernel object leaked once
// per job, and the leak ends in ENOSPC on the cgroup hierarchy.
func (g *Group) remove() error {
	const attempts = 100
	var err error
	for range attempts {
		if err = os.Remove(g.path); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("cgroup: remove %s: %w", g.path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("cgroup: remove %s: still busy after 1s: %w", g.path, err)
}
