//go:build linux

package syscalls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/parth-sharma-10/forge/internal/metrics"
	"github.com/parth-sharma-10/forge/internal/sandbox"
)

// TestMain takes the re-executed sandbox child path, exactly as the sandbox
// package's own tests do: sandbox.Run re-execs this test binary to build the
// isolation, and without this it would run the suite again as the child.
func TestMain(m *testing.M) {
	sandbox.MaybeChild()
	os.Exit(m.Run())
}

// TestSyscallsAreCountedPerJobAndDoNotMix runs two real sandboxed builds with
// deliberately different syscall signatures and checks each is attributed to
// its own cgroup: an openat-heavy build and a write-heavy one. The proof of
// the cgroup filter is that the write-heavy build's openat count stays small
// even though this test process is making thousands of openats the whole time
// — if attribution counted the machine rather than the job, that count would
// be huge.
func TestSyscallsAreCountedPerJobAndDoNotMix(t *testing.T) {
	rootfs := os.Getenv("FORGE_TEST_ROOTFS")
	if os.Geteuid() != 0 || rootfs == "" {
		t.Skip("needs root and FORGE_TEST_ROOTFS: run `make test-syscalls`")
	}

	var reg metrics.Registry
	obs, err := Load(&reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		// In CI the load is a gate: the whole point of the feature is that it
		// loads. On a developer's machine that cannot, it is an optional extra.
		if os.Getenv("CI") != "" {
			t.Fatalf("Load: %v", err)
		}
		t.Skipf("BPF unavailable: %v", err)
	}
	defer obs.Close()

	root := mkCgroupRoot(t)
	run := func(t *testing.T, script string) {
		t.Helper()
		err := sandbox.Run(context.Background(), sandbox.Spec{
			Rootfs: rootfs, Args: []string{"/bin/sh", "-c", script}, CgroupRoot: root,
			Observe: obs.Job(), Stdout: io.Discard, Stderr: io.Discard,
		})
		var exit *exec.ExitError
		if err != nil && errors.As(err, &exit) && exit.ExitCode() == sandbox.SetupExitCode {
			t.Fatalf("sandbox setup failed: %v", err)
		}
	}

	write := uint64(unix.SYS_WRITE)

	// Both workloads run entirely in the build's PID 1 shell — the enrolled
	// cgroup — so the counts come from the process the tracepoint attributes,
	// not from a forked child whose exec might behave differently in the image.
	// The two are asserted by the *shape* of their histograms — one dominated
	// by writes, one not — rather than by a single hard-coded syscall number,
	// because which number a musl/busybox shell uses to open a file (open vs
	// openat) is not ours to assume.
	//
	// file-heavy: a redirect the shell opens and reads 500 times, few writes.
	before := snapshotAll(obs)
	run(t, "i=0; while [ $i -lt 500 ]; do read x < /etc/passwd; i=$((i+1)); done")
	a := deltaAll(snapshotAll(obs), before)

	// write-heavy: a shell-builtin write, 500 times, opening nothing.
	before = snapshotAll(obs)
	run(t, "i=0; while [ $i -lt 500 ]; do echo x; i=$((i+1)); done")
	b := deltaAll(snapshotAll(obs), before)

	t.Logf("file-heavy histogram:  %s", top(a))
	t.Logf("write-heavy histogram: %s", top(b))

	totalA, totalB := total(a), total(b)

	// The write-heavy job's writes are counted, per job.
	if b[write] < 200 {
		t.Errorf("write-heavy job counted %d write, want >= 200", b[write])
	}
	// ...and dominate its own histogram: it is a write workload.
	if b[write] < totalB*6/10 {
		t.Errorf("write-heavy job: write %d is not the bulk of its %d syscalls", b[write], totalB)
	}
	// The cgroup filter attributes to the job, not the machine: the write-heavy
	// job opens nothing, so its whole non-write count stays small even though
	// this test process is making thousands of syscalls of every kind
	// throughout. A broken filter would show the machine's traffic here.
	if nonWrite := totalB - b[write]; nonWrite > 150 {
		t.Errorf("write-heavy job counted %d non-write syscalls, want < 150: the filter is leaking the machine's syscalls into the job", nonWrite)
	}
	// The file-heavy job did real in-process work, and it is not a write
	// workload — a distinct signature from the write-heavy job.
	if totalA < 200 {
		t.Errorf("file-heavy job counted %d syscalls, want >= 200", totalA)
	}
	if a[write] >= b[write] {
		t.Errorf("the two jobs' write counts did not separate: file-heavy %d, write-heavy %d", a[write], b[write])
	}
	if totalA-a[write] < 200 {
		t.Errorf("file-heavy job's work is not non-write: %d of %d were writes", a[write], totalA)
	}
}

// snapshotAll reads every syscall counter's current value.
func snapshotAll(o *Observer) map[uint64]int64 {
	m := make(map[uint64]int64, len(o.counters))
	o.mu.Lock()
	defer o.mu.Unlock()
	for nr, c := range o.counters {
		m[nr] = c.Value()
	}
	return m
}

func deltaAll(after, before map[uint64]int64) map[uint64]int64 {
	d := make(map[uint64]int64, len(after))
	for k, v := range after {
		if n := v - before[k]; n != 0 {
			d[k] = n
		}
	}
	return d
}

func total(m map[uint64]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}

// top renders a histogram's largest entries, for the test log.
func top(m map[uint64]int64) string {
	type kv struct {
		nr uint64
		n  int64
	}
	s := make([]kv, 0, len(m))
	for nr, n := range m {
		s = append(s, kv{nr, n})
	}
	sort.Slice(s, func(i, j int) bool { return s[i].n > s[j].n })
	var b strings.Builder
	for i, e := range s {
		if i >= 12 {
			fmt.Fprintf(&b, " …(%d more)", len(s)-i)
			break
		}
		fmt.Fprintf(&b, " nr%d=%d", e.nr, e.n)
	}
	return b.String()
}

func mkCgroupRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/sys/fs/cgroup", "forge-syscalls-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(root) })
	return root
}
