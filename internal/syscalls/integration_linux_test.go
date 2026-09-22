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

	read, write := uint64(unix.SYS_READ), uint64(unix.SYS_WRITE)

	// Both workloads run entirely in the build's PID 1 shell — the enrolled
	// cgroup — so the counts come from the process the tracepoint attributes,
	// not from a forked child whose exec might behave differently in the image.
	// They are told apart by read vs write, the two syscalls a shell issues by
	// their own numbers whatever else it does (a musl/busybox shell opens a
	// file with open on amd64, not openat, so the open number is not ours to
	// assume — read and write are).
	//
	// read-heavy: a redirect the shell reads through 500 times.
	before := snapshotAll(obs)
	run(t, "i=0; while [ $i -lt 500 ]; do read x < /etc/passwd; i=$((i+1)); done")
	a := deltaAll(snapshotAll(obs), before)

	// write-heavy: a shell-builtin write 500 times, reading nothing.
	before = snapshotAll(obs)
	run(t, "i=0; while [ $i -lt 500 ]; do echo x; i=$((i+1)); done")
	b := deltaAll(snapshotAll(obs), before)

	t.Logf("read-heavy histogram:  %s", top(a))
	t.Logf("write-heavy histogram: %s", top(b))

	// Each job's own dominant syscall is counted, per job.
	if a[read] < 1000 {
		t.Errorf("read-heavy job counted %d read, want >= 1000", a[read])
	}
	if b[write] < 200 {
		t.Errorf("write-heavy job counted %d write, want >= 200", b[write])
	}
	// The signatures do not mix: the read-heavy job reads far more than the
	// write-heavy one, and vice versa for writes.
	if a[read] <= b[read] {
		t.Errorf("read did not separate the jobs: read-heavy %d, write-heavy %d", a[read], b[read])
	}
	if b[write] <= a[write] {
		t.Errorf("write did not separate the jobs: write-heavy %d, read-heavy %d", b[write], a[write])
	}
	// The cgroup filter attributes to the job, not the machine: the write-heavy
	// job barely reads, so its read count stays small even though this test
	// process reads constantly throughout. A filter counting the machine would
	// show that traffic here.
	if b[read] > 200 {
		t.Errorf("write-heavy job counted %d read, want < 200: the filter is leaking the machine's syscalls into the job", b[read])
	}
}

// TestADeniedSyscallIsNotCounted pins down what the histogram can see. The
// kernel runs the seccomp filter before the sys_enter tracepoint, and a call
// the filter denies returns without reaching it, so a build hammering a
// denied syscall adds nothing for it here. What the histogram does count is
// Forge's own sandbox setup, which runs in the job's cgroup before the filter
// is installed: its mounts (the first CI run counted exactly eleven — private
// remount, scratch tmpfs, overlay, proc, the /dev tmpfs, six device binds) and
// its pivot_root.
//
// So a job that tries mount two hundred times is compared with one that never
// tries: the two mount counts must be equal, both being the setup's. The
// control is execve: each attempt forks and execs the mount applet, so a count
// at least the loop's length proves the attempts ran and were attributed, and
// equal mount counts then mean "not visible" rather than "not run".
func TestADeniedSyscallIsNotCounted(t *testing.T) {
	rootfs := os.Getenv("FORGE_TEST_ROOTFS")
	if os.Geteuid() != 0 || rootfs == "" {
		t.Skip("needs root and FORGE_TEST_ROOTFS: run `make test-syscalls`")
	}
	var reg metrics.Registry
	obs, err := Load(&reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("Load: %v", err)
		}
		t.Skipf("BPF unavailable: %v", err)
	}
	defer obs.Close()

	root := mkCgroupRoot(t)
	run := func(script string) map[uint64]int64 {
		t.Helper()
		before := snapshotAll(obs)
		var out strings.Builder
		err := sandbox.Run(context.Background(), sandbox.Spec{
			Rootfs: rootfs, CgroupRoot: root, Observe: obs.Job(),
			Args: []string{"/bin/sh", "-c", script}, Stdout: &out, Stderr: &out,
		})
		t.Logf("%q: err %v, output %q", script[:min(len(script), 40)], err, strings.TrimSpace(out.String()))
		return deltaAll(snapshotAll(obs), before)
	}

	const tries = 200
	quiet := run("true")
	loud := run(fmt.Sprintf("i=0; while [ $i -lt %d ]; do mount -t tmpfs t /mnt 2>/dev/null; i=$((i+1)); done; mount -t tmpfs t /mnt", tries))
	t.Logf("no attempts:   %s", top(quiet))
	t.Logf("%d attempts: %s", tries, top(loud))

	mount, execve, pivot := uint64(unix.SYS_MOUNT), uint64(unix.SYS_EXECVE), uint64(unix.SYS_PIVOT_ROOT)
	if loud[execve] < tries {
		t.Fatalf("execve counted %d, want >= %d: the mount attempts did not run or were not attributed, so equal mount counts would prove nothing", loud[execve], tries)
	}
	if quiet[mount] == 0 || quiet[pivot] == 0 {
		t.Fatalf("the setup's mount (%d) and pivot_root (%d) were not counted: the histogram is expected to include Forge's own sandbox setup", quiet[mount], quiet[pivot])
	}
	if loud[mount] != quiet[mount] {
		t.Fatalf("mount counted %d with %d denied attempts and %d with none: denied calls reached the tracepoint", loud[mount], tries, quiet[mount])
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
