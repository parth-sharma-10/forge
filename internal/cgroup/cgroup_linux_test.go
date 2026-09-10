//go:build linux

package cgroup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testRoot is a parent cgroup of our own, so a failing test cannot disturb the
// machine's real one. It skips rather than fails when the environment cannot
// support cgroups at all.
func testRoot(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("cgroup tests need root")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skipf("cgroups v2 is not mounted at /sys/fs/cgroup: %v", err)
	}
	// MkdirTemp, not a name built from the pid: pids recycle, and a root left
	// behind by an earlier run comes with its subtree_control already
	// configured — which silently does prepareRoot's job for it and hides
	// whether prepareRoot still works.
	root, err := os.MkdirTemp("/sys/fs/cgroup", "forge-test-")
	if err != nil {
		t.Fatal(err)
	}
	// Created by MkdirTemp, so Create must find it preexisting and leave it;
	// removing it is this helper's job.
	t.Cleanup(func() {
		// Removable only if every job cgroup under it was cleaned up.
		if err := os.Remove(root); err != nil && !os.IsNotExist(err) {
			t.Errorf("test root not empty at the end of the test: %v", err)
		}
	})
	return root
}

// closeGroup fails the test if the cgroup could not be removed. Discarding
// this error is how a leak stays invisible: the assertions still pass, and the
// job cgroup is simply still there afterwards.
func closeGroup(t *testing.T, g *Group) {
	t.Helper()
	if err := g.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// stop kills a process and reaps it, so its cgroup can be removed.
func stop(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	cmd.Process.Kill()
	cmd.Wait()
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestCreateWritesLimits(t *testing.T) {
	root := testRoot(t)
	limits := Limits{MemoryBytes: 64 << 20, CPUCores: 0.5, MaxPIDs: 32}

	g, err := Create(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer closeGroup(t, g)

	want := map[string]string{
		"memory.max":      "67108864",
		"memory.swap.max": "0",
		"cpu.max":         "50000 100000",
		"pids.max":        "32",
	}
	for name, value := range want {
		if got := read(t, filepath.Join(g.Path(), name)); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

func TestCreateRejectsAnUndelegatedRoot(t *testing.T) {
	// A cgroup nested under a job cgroup gets no controllers, because nothing
	// enabled them in its parent's subtree_control.
	root := testRoot(t)
	g, err := Create(root, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeGroup(t, g)

	if _, err := Create(filepath.Join(g.Path(), "nested"), Limits{MemoryBytes: 1 << 20}); err == nil {
		t.Fatal("Create succeeded under a root with no delegated controllers, want an error naming the missing controller")
	} else if !strings.Contains(err.Error(), "not delegated") {
		t.Fatalf("err = %v, want it to name the undelegated controller", err)
	}
}

func TestCloseRemovesTheCgroup(t *testing.T) {
	root := testRoot(t)
	g, err := Create(root, Limits{MemoryBytes: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	path := g.Path()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cgroup does not exist after Create: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cgroup still exists after Close: %v", err)
	}
}

// startInGroup runs a command placed in g by the kernel at clone time, the same
// way the sandbox does.
func startInGroup(t *testing.T, g *Group, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: g.FD()}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestProcessIsBornInTheCgroup(t *testing.T) {
	root := testRoot(t)
	g, err := Create(root, Limits{MaxPIDs: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer closeGroup(t, g)

	cmd := startInGroup(t, g, "/bin/sleep", "30")
	defer stop(t, cmd)

	procs := read(t, filepath.Join(g.Path(), "cgroup.procs"))
	if procs != strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("cgroup.procs = %q, want the child's pid %d", procs, cmd.Process.Pid)
	}
}

// TestCPULimitThrottles is the check that the number written to cpu.max means
// something: a busy loop under a tenth of a core must accumulate throttled
// time. Without the limit the counter stays at zero however long it spins.
func TestCPULimitThrottles(t *testing.T) {
	root := testRoot(t)
	g, err := Create(root, Limits{CPUCores: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeGroup(t, g)

	defer stop(t, startInGroup(t, g, "/bin/sh", "-c", "while :; do :; done"))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if throttled(t, g) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("a busy loop capped at 0.1 cores was never throttled:\n%s", read(t, filepath.Join(g.Path(), "cpu.stat")))
}

func throttled(t *testing.T, g *Group) int64 {
	t.Helper()
	for _, line := range strings.Split(read(t, filepath.Join(g.Path(), "cpu.stat")), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || key != "throttled_usec" {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatalf("parse throttled_usec %q: %v", value, err)
		}
		return n
	}
	t.Fatal("cpu.stat has no throttled_usec")
	return 0
}

func TestPIDLimitStopsForking(t *testing.T) {
	root := testRoot(t)
	g, err := Create(root, Limits{MaxPIDs: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer closeGroup(t, g)

	// More background processes than the limit allows. The shell survives; the
	// forks past the cap fail. It waits for the ones that started, so the
	// cgroup is empty by the time Close removes it.
	cmd := exec.Command("/bin/sh", "-c", "for i in 1 2 3 4 5 6 7 8 9 10; do sleep 0.2 & done; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: g.FD()}
	cmd.Run()

	if got := read(t, filepath.Join(g.Path(), "pids.events")); !strings.Contains(got, "max ") {
		t.Fatalf("pids.events = %q, want a max event recorded", got)
	}
	if got := read(t, filepath.Join(g.Path(), "pids.peak")); got != "4" {
		t.Fatalf("pids.peak = %q, want 4: the cap was not enforced", got)
	}
}

func TestOOMKilledIsFalseForAHealthyJob(t *testing.T) {
	root := testRoot(t)
	g, err := Create(root, Limits{MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer closeGroup(t, g)

	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: g.FD()}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	oom, err := g.OOMKilled()
	if err != nil {
		t.Fatal(err)
	}
	if oom {
		t.Fatal("OOMKilled reported a kill for a command that exited cleanly")
	}
}
