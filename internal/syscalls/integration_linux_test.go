//go:build linux

package syscalls

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
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

	openat, write := uint64(unix.SYS_OPENAT), uint64(unix.SYS_WRITE)

	before := snapshot(obs, openat, write)
	run(t, "i=0; while [ $i -lt 300 ]; do cat /etc/passwd >/dev/null; i=$((i+1)); done")
	a := delta(snapshot(obs, openat, write), before)

	before = snapshot(obs, openat, write)
	run(t, "i=0; while [ $i -lt 1000 ]; do echo x; i=$((i+1)); done")
	b := delta(snapshot(obs, openat, write), before)

	t.Logf("openat-heavy: openat=%d write=%d | write-heavy: openat=%d write=%d",
		a[openat], a[write], b[openat], b[write])

	if a[openat] < 200 {
		t.Errorf("openat-heavy job counted %d openat, want >= 200", a[openat])
	}
	if b[openat] > 100 {
		t.Errorf("write-heavy job counted %d openat, want < 100: attribution is leaking the machine's syscalls into the job", b[openat])
	}
	if a[openat] <= b[openat] {
		t.Errorf("openat: heavy job %d is not clearly above light job %d", a[openat], b[openat])
	}
	if b[write] < 200 {
		t.Errorf("write-heavy job counted %d write, want >= 200", b[write])
	}
	if b[write] <= a[write] {
		t.Errorf("write: heavy job %d is not above light job %d", b[write], a[write])
	}
}

// snapshot reads the current counter value for each syscall number.
func snapshot(o *Observer, nrs ...uint64) map[uint64]int64 {
	m := make(map[uint64]int64, len(nrs))
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, nr := range nrs {
		if c := o.counters[nr]; c != nil {
			m[nr] = c.Value()
		}
	}
	return m
}

func delta(after, before map[uint64]int64) map[uint64]int64 {
	d := make(map[uint64]int64, len(after))
	for k, v := range after {
		d[k] = v - before[k]
	}
	return d
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
