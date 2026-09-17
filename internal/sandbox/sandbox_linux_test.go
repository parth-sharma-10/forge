//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/parth-sharma-10/forge/internal/cgroup"
)

// TestMain gives the test binary the same dual identity every Forge binary
// has: Run re-executes /proc/self/exe, which here is the test binary itself,
// so it must take the child path instead of running the suite again.
func TestMain(m *testing.M) {
	MaybeChild()
	if len(os.Args) > 2 && os.Args[1] == probeArg {
		probe(os.Args[2])
	}
	os.Exit(m.Run())
}

// probeArg marks the test binary's third identity: the build command itself,
// copied into a scratch image and run inside a sandbox, for the properties
// that need a raw syscall no shell utility issues.
const probeArg = "forge-test-probe"

// probe issues one raw syscall and prints what the kernel said.
func probe(what string) {
	var flags uintptr
	switch what {
	case "clone-fork":
		flags = uintptr(syscall.SIGCHLD)
	case "clone-newuser":
		flags = syscall.CLONE_NEWUSER | uintptr(syscall.SIGCHLD)
	case "raw-socket":
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_ICMP)
		if err != nil {
			fmt.Println("socket:", err)
		} else {
			syscall.Close(fd)
			fmt.Println("socket: ok")
		}
		os.Exit(0)
	default:
		fmt.Println("probe: unknown", what)
		os.Exit(2)
	}
	// RawSyscall, and nothing but a raw exit in the child: the child is a
	// single-threaded copy of a Go process and must not touch the runtime.
	pid, _, errno := syscall.RawSyscall(syscall.SYS_CLONE, flags, 0, 0)
	if errno != 0 {
		fmt.Println("clone:", errno)
		os.Exit(0)
	}
	if pid == 0 {
		syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 0, 0, 0)
	}
	var status syscall.WaitStatus
	if _, err := syscall.Wait4(int(pid), &status, 0, nil); err != nil {
		fmt.Println("wait4:", err)
		os.Exit(0)
	}
	fmt.Println("clone: ok")
	os.Exit(0)
}

// runProbe copies the test binary into a private image and runs it there as
// the build, in probe mode.
func runProbe(t *testing.T, what string) string {
	t.Helper()
	dir := scratchImage(t)
	self, err := os.ReadFile("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "forge-probe"), self, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runSpec(t, Spec{Rootfs: dir, Args: []string{"/forge-probe", probeArg, what}})
	if err != nil {
		t.Fatalf("probe %s: %v (output %q)", what, err, out)
	}
	return out
}

const rootfsEnv = "FORGE_TEST_ROOTFS"

// rootfs returns the extracted root filesystem the sandbox tests run against,
// skipping when the environment cannot host them. See `make rootfs`.
func rootfs(t *testing.T) string {
	t.Helper()
	dir := os.Getenv(rootfsEnv)
	if dir == "" {
		t.Skipf("%s not set: run `make test-sandbox`", rootfsEnv)
	}
	if os.Geteuid() != 0 {
		t.Skip("sandbox tests need root: no user namespace yet, so CAP_SYS_ADMIN is required")
	}
	// Lstat, not Stat: /bin/sh in an Alpine rootfs is an absolute symlink to
	// /bin/busybox, which resolves against the host root and so is dangling
	// right up until pivot_root makes it resolve inside the sandbox.
	if _, err := os.Lstat(filepath.Join(dir, "bin/sh")); err != nil {
		t.Fatalf("%s=%s does not look like a root filesystem: %v", rootfsEnv, dir, err)
	}
	// The overlay copies a file up only if the sandbox can name its owner,
	// and host root is the one id the map leaves out. hack/shift-rootfs.sh
	// moves the image into the map; the check is here so the two constants
	// cannot drift apart silently.
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	if st.Uid != mappedBase {
		t.Fatalf("%s is owned by uid %d, want %d: run `sudo hack/shift-rootfs.sh %s`", dir, st.Uid, mappedBase, dir)
	}
	// A bind mount left on the host by an earlier broken run satisfies
	// pivot_root's mount-point requirement for free, so the suite would pass
	// without the code that is supposed to create it. Refuse to run dirty.
	if mountedOnHost(t, dir) {
		t.Fatalf("%s is already mounted in the host namespace: a previous run leaked it. `sudo umount %s` and re-run", dir, dir)
	}
	return dir
}

// mountedOnHost reports whether dir is a mount point in the caller's mount
// namespace, which for the test process is the host's.
func mountedOnHost(t *testing.T, dir string) bool {
	t.Helper()
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(mountinfo), "\n") {
		if strings.Contains(line, " "+dir+" ") {
			return true
		}
	}
	return false
}

// sh runs a shell command inside a sandbox and returns its combined output.
func sh(t *testing.T, script string) (string, error) {
	t.Helper()
	return runArgs(t, "/bin/sh", "-c", script)
}

// shIn is sh against a given root filesystem rather than the shared image.
func shIn(t *testing.T, dir, script string) (string, error) {
	t.Helper()
	return runSpec(t, Spec{Rootfs: dir, Hostname: "forge-test", Args: []string{"/bin/sh", "-c", script}})
}

// scratchImage is a private copy of the image for a test whose build writes
// outside /tmp. With the overlay in place the copy is never touched; without
// it — which is what the mutation harness checks — the writes land here and
// not in the image every other test shares. cp -a keeps the shifted owners.
//
// Not under t.TempDir(): that is mode 0700 and owned by host root, which the
// sandbox does not map, so the child could not reach the image inside it.
func scratchImage(t *testing.T) string {
	t.Helper()
	parent, err := os.MkdirTemp("", "forge-image-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "rootfs")
	if out, err := exec.Command("cp", "-a", rootfs(t), dir).CombinedOutput(); err != nil {
		t.Fatalf("cp -a: %v: %s", err, out)
	}
	return dir
}

func runArgs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runSpec(t, Spec{Rootfs: rootfs(t), Hostname: "forge-test", Args: args})
}

// runSpec runs spec with its output captured, and returns that output trimmed.
func runSpec(t *testing.T, spec Spec) (string, error) {
	t.Helper()
	var out bytes.Buffer
	spec.Stdout, spec.Stderr = &out, &out
	err := Run(context.Background(), spec)
	return strings.TrimSpace(out.String()), err
}

func TestCommandRunsAsPIDOne(t *testing.T) {
	out, err := sh(t, "echo $$")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if out != "1" {
		t.Fatalf("pid inside sandbox = %q, want 1", out)
	}
}

func TestProcIsTheSandboxProcessTable(t *testing.T) {
	// Counting entries under /proc would be racy — the shell forks for each
	// command in the script — so the assertion is that a process known to
	// exist on the host has no entry at all inside.
	hostPID := os.Getpid()
	out, err := sh(t, fmt.Sprintf("cat /proc/1/comm; test -e /proc/%d && echo LEAKED || echo ISOLATED", hostPID))
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if want := "sh\nISOLATED"; out != want {
		t.Fatalf("got %q, want %q: /proc is not the sandbox's own view (host pid %d)", out, want, hostPID)
	}
}

func TestHostnameIsIsolated(t *testing.T) {
	hostBefore, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}

	out, err := runArgs(t, "/bin/hostname")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if out != "forge-test" {
		t.Fatalf("hostname inside sandbox = %q, want forge-test", out)
	}

	hostAfter, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if hostAfter != hostBefore {
		t.Fatalf("host hostname changed from %q to %q: the UTS namespace did not hold", hostBefore, hostAfter)
	}
}

func TestHostFilesystemIsUnreachable(t *testing.T) {
	marker, err := os.CreateTemp("", "forge-host-marker-")
	if err != nil {
		t.Fatal(err)
	}
	marker.Close()
	defer os.Remove(marker.Name())

	// This is also the assertion behind detaching the old root. pivot_root
	// parks it stacked over the new one, so a missing unmount does not leave a
	// directory to look for — it leaves the sandbox looking at the host's
	// filesystem as /, and the marker visible.
	out, err := sh(t, "test -e "+marker.Name()+" && echo VISIBLE || echo HIDDEN")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if out != "HIDDEN" {
		t.Fatalf("got %q, want HIDDEN: the host root is still reachable from inside", out)
	}
}

func TestNetworkNamespaceHasNoHostInterfaces(t *testing.T) {
	out, err := sh(t, "cat /proc/net/dev")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "lo:") {
		t.Fatalf("no loopback in /proc/net/dev, so this is not a network namespace:\n%s", out)
	}
	for _, iface := range []string{"eth0", "docker0", "wlan0"} {
		if strings.Contains(out, iface+":") {
			t.Fatalf("host interface %s visible inside the sandbox:\n%s", iface, out)
		}
	}
}

// TestEveryNamespaceIsNew checks the identity of all five namespaces at once:
// a namespace's inode number under /proc/self/ns differs from the host's only
// if the process really is in a different one. It catches a clone flag that
// was dropped but whose behavioural effect another test does not cover — IPC
// has no cheap observable otherwise.
func TestEveryNamespaceIsNew(t *testing.T) {
	kinds := []string{"mnt", "uts", "ipc", "pid", "net"}

	host := make(map[string]string, len(kinds))
	for _, kind := range kinds {
		link, err := os.Readlink("/proc/self/ns/" + kind)
		if err != nil {
			t.Fatal(err)
		}
		host[kind] = link
	}

	out, err := sh(t, "for n in "+strings.Join(kinds, " ")+"; do readlink /proc/self/ns/$n; done")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	inside := strings.Split(out, "\n")
	if len(inside) != len(kinds) {
		t.Fatalf("read %d namespaces, want %d:\n%s", len(inside), len(kinds), out)
	}
	for i, kind := range kinds {
		if inside[i] == host[kind] {
			t.Errorf("%s namespace is the host's (%s)", kind, host[kind])
		}
	}
}

// TestMountsDoNotLeakToHost is the assertion behind the private-propagation
// call: without it the rootfs bind mount propagates into the host's peer group
// and outlives the sandbox that made it.
func TestMountsDoNotLeakToHost(t *testing.T) {
	dir := rootfs(t) // fails already if the host is dirty going in
	if out, err := sh(t, "true"); err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if mountedOnHost(t, dir) {
		t.Fatalf("the sandbox bind mount of %s survived in the host mount namespace", dir)
	}
}

// devScript exercises each node by what it does rather than by its major and
// minor numbers: a node made from the wrong pair still passes an `ls -l`
// assertion on the numbers this test would have hard-coded, but /dev/full does
// not refuse a write unless it really is 1:7.
const devScript = `
for n in null zero full random urandom tty; do [ -c /dev/$n ] || echo "MISSING $n"; done
echo discarded > /dev/null
[ -s /dev/null ] && echo null-is-a-file || echo null-discards
dd if=/dev/zero bs=4 count=1 2>/dev/null | wc -c
dd if=/dev/urandom bs=16 count=1 2>/dev/null | wc -c
echo x > /dev/full 2>/dev/null && echo full-accepted || echo full-refused
echo piped | cat /dev/stdin
ls /dev/fd > /dev/null && echo fd-ok
`

func TestDevNodesBehaveLikeTheirKernelDevices(t *testing.T) {
	out, err := sh(t, devScript)
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	want := "null-discards\n4\n16\nfull-refused\npiped\nfd-ok"
	if out != want {
		t.Fatalf("got:\n%s\n\nwant:\n%s", out, want)
	}
}

// TestDevNodesAreWritableByAnyone fails if the nodes ever stop being bind
// mounts of the host's. A bind presents the source inode, so /dev/null is 0666
// because the host's is; anything that recreated these as files on the tmpfs
// would present that file's mode instead, and work for exactly as long as every
// build runs as uid 0 inside.
func TestDevNodesAreWritableByAnyone(t *testing.T) {
	out, err := sh(t, "ls -l /dev/null | cut -c1-10")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if out != "crw-rw-rw-" {
		t.Fatalf("/dev/null mode = %q, want crw-rw-rw-", out)
	}
}

// TestABackgroundJobPrintsNothing is the symptom that sent us here: sh opens
// /dev/null for every job it backgrounds, so before there was a /dev, any
// build script containing `&` printed an error per `&`.
func TestABackgroundJobPrintsNothing(t *testing.T) {
	out, err := sh(t, "sleep 0 & wait; echo done")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if out != "done" {
		t.Fatalf("output = %q, want just \"done\": backgrounding a job still writes to stderr", out)
	}
}

// TestDevIsOnlyTheMinimalSet fails if anyone ever "fixes" a missing device by
// bind-mounting the host's /dev in. A sandbox that can open /dev/sda or
// /dev/mem has no boundary left worth the name.
func TestDevIsOnlyTheMinimalSet(t *testing.T) {
	out, err := sh(t, "ls -a /dev | sort | tr '\\n' ' '")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	want := ". .. fd full null random stderr stdin stdout tty urandom zero"
	if out != want {
		t.Fatalf("/dev = %q, want %q", out, want)
	}
}

// TestDevDoesNotOutliveTheSandbox covers the cleanup path, which is the mount
// namespace's rather than any code's: nothing unmounts /dev, so the test is
// that nothing has to. It runs a build that is killed rather than one that
// exits, since a clean exit is the case least likely to leave anything behind.
func TestDevDoesNotOutliveTheSandbox(t *testing.T) {
	dir := rootfs(t)
	dev := filepath.Join(dir, "dev")
	if dirt := emptyDir(t, dev); len(dirt) != 0 {
		t.Logf("%s was not empty before the run (%v) — a manual run or a crash left it; cleared", dev, dirt)
	}

	spec := Spec{Rootfs: dir, Args: []string{"/bin/sh", "-c", "echo contaminated > /dev/null; sleep 60"}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, spec) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sandbox outlived its cancelled context")
	}

	if mountedOnHost(t, dev) {
		t.Fatalf("the sandbox's tmpfs on %s survived in the host mount namespace", dev)
	}
	// A `> /dev/null` must not leave a regular file behind in the image. The
	// overlay guarantees that on its own now; the tmpfs under /dev did before
	// it, and still keeps the nodes out of the job's own root filesystem.
	if leaked := emptyDir(t, dev); len(leaked) != 0 {
		t.Fatalf("%s is not empty after the run (%v): the build wrote into the rootfs image", dev, leaked)
	}
}

// emptyDir removes everything in dir and returns what it removed.
//
// The removal is the point, not a convenience. This assertion is about the
// rootfs image being clean, and an assertion that leaves the image dirty when
// it fails makes every later run in the same sandbox fail for a reason that
// has nothing to do with the code under test — as one leftover /dev/null makes
// the next run's mknod fail with EEXIST, taking the whole suite with it.
func emptyDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			t.Errorf("clearing %s: %v", dir, err)
		}
	}
	return names
}

func TestExitStatusPropagates(t *testing.T) {
	_, err := sh(t, "exit 7")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want *exec.ExitError", err)
	}
	if got := exitErr.ExitCode(); got != 7 {
		t.Fatalf("exit code = %d, want 7", got)
	}
}

func TestSetupFailureIsDistinguishable(t *testing.T) {
	rootfs(t) // same environment requirements, but deliberately not that rootfs
	var out bytes.Buffer
	err := Run(context.Background(), Spec{
		Rootfs: t.TempDir(), // a valid directory with no /bin/sh in it
		Args:   []string{"/bin/sh", "-c", "true"},
		Stderr: &out,
	})

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want *exec.ExitError", err)
	}
	if got := exitErr.ExitCode(); got != SetupExitCode {
		t.Fatalf("exit code = %d, want %d (setup failure must not look like the command's status)", got, SetupExitCode)
	}
	if !strings.Contains(out.String(), "sandbox:") {
		t.Fatalf("setup failure gave no reason on stderr: %q", out.String())
	}
}

func TestContextCancelKillsTheSandbox(t *testing.T) {
	spec := Spec{Rootfs: rootfs(t), Args: []string{"/bin/sh", "-c", "sleep 60"}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, spec) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sandbox outlived its cancelled context")
	}
}

// memoryHog allocates without bound until the kernel stops it.
const memoryHog = "x=aaaaaaaaaaaaaaaa; while :; do x=$x$x; done; echo survived"

// testCgroupRoot keeps each test's job cgroups under a parent of our own, so a
// failure cannot leave debris in the tree the machine actually uses.
func testCgroupRoot(t *testing.T) string {
	t.Helper()
	// Three tests call this before rootfs(t), and a skip only guards what runs
	// after it — so the environment check has to be here too, not only there.
	rootfs(t)
	// MkdirTemp, not a name built from the pid: pids recycle, and a root a
	// previous run left behind arrives with its controllers already enabled.
	root, err := os.MkdirTemp("/sys/fs/cgroup", "forge-sandbox-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Removable only if every job cgroup under it was cleaned up.
		if err := os.Remove(root); err != nil && !os.IsNotExist(err) {
			t.Errorf("test cgroup root not empty at the end of the test: %v", err)
		}
	})
	return root
}

// TestMemoryLimitIsReportedAsAnOOMKill is the assertion behind reading
// memory.events at all. The exit status of an OOM-killed build is SIGKILL,
// which is also what a timeout and an operator's kill -9 look like; only the
// cgroup can say which happened.
func TestMemoryLimitIsReportedAsAnOOMKill(t *testing.T) {
	var out bytes.Buffer
	err := Run(context.Background(), Spec{
		Rootfs:     rootfs(t),
		CgroupRoot: testCgroupRoot(t),
		Limits:     cgroup.Limits{MemoryBytes: 16 << 20},
		// Doubling a shell variable, rather than reading /dev/zero into one:
		// a pipe-fed workload charges page cache to the same cgroup, so the
		// kernel reclaims busybox's own pages and thrashes for seconds before
		// it OOMs. Doubling is pure anonymous memory and reaches the cap in
		// about twenty iterations.
		Args:   []string{"/bin/sh", "-c", memoryHog},
		Stdout: &out,
		Stderr: &out,
	})

	if !errors.Is(err, ErrOOMKilled) {
		t.Fatalf("err = %v, want it to wrap ErrOOMKilled (output %q)", err, out.String())
	}
	// The error must still carry the status, so a caller that only knows about
	// exit codes is not left with nothing.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want it to also unwrap to *exec.ExitError", err)
	}
	if strings.Contains(out.String(), "survived") {
		t.Fatalf("the build ran to completion despite a 16MiB limit: %q", out.String())
	}
}

func TestAJobWithinItsMemoryLimitIsNotReportedAsOOM(t *testing.T) {
	out, err := runSpec(t, Spec{
		Rootfs:     rootfs(t),
		CgroupRoot: testCgroupRoot(t),
		Limits:     cgroup.Limits{MemoryBytes: 64 << 20, MaxPIDs: 32},
		Args:       []string{"/bin/sh", "-c", "x=aaaaaaaaaaaaaaaa; for i in 1 2 3 4 5 6; do x=$x$x; done; echo survived"},
	})
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if out != "survived" {
		t.Fatalf("output = %q, want survived", out)
	}
}

func TestSandboxRunsInsideItsOwnCgroup(t *testing.T) {
	root := testCgroupRoot(t)
	out, err := runSpec(t, Spec{
		Rootfs:     rootfs(t),
		CgroupRoot: root,
		Limits:     cgroup.Limits{MemoryBytes: 64 << 20},
		// /proc is the sandbox's own mount, and without a cgroup namespace
		// this reports the full path from the cgroup root.
		Args: []string{"/bin/sh", "-c", "cat /proc/self/cgroup"},
	})
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	want := strings.TrimPrefix(root, "/sys/fs/cgroup") + "/job-"
	if !strings.Contains(out, want) {
		t.Fatalf("/proc/self/cgroup = %q, want it under %q", out, want)
	}
}

func TestCgroupIsRemovedAfterTheJob(t *testing.T) {
	root := testCgroupRoot(t)
	if out, err := runSpec(t, Spec{
		Rootfs:     rootfs(t),
		CgroupRoot: root,
		Limits:     cgroup.Limits{MemoryBytes: 64 << 20},
		Args:       []string{"/bin/sh", "-c", "true"},
	}); err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "job-") {
			t.Fatalf("job cgroup %s survived the run: one leaked kernel object per build", e.Name())
		}
	}
}

// TestCgroupIsRemovedAfterAnOOMKill covers the path where the job did not exit
// on its own terms — the cleanup has to survive the kernel still reaping it.
func TestCgroupIsRemovedAfterAnOOMKill(t *testing.T) {
	root := testCgroupRoot(t)
	err := Run(context.Background(), Spec{
		Rootfs:     rootfs(t),
		CgroupRoot: root,
		Limits:     cgroup.Limits{MemoryBytes: 16 << 20},
		Args:       []string{"/bin/sh", "-c", memoryHog},
	})
	if !errors.Is(err, ErrOOMKilled) {
		t.Fatalf("err = %v, want ErrOOMKilled", err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "job-") {
			t.Fatalf("job cgroup %s survived an OOM-killed run", e.Name())
		}
	}
}

func TestInvalidLimitsAreRejectedBeforeAnythingRuns(t *testing.T) {
	err := Run(context.Background(), Spec{
		Rootfs: t.TempDir(),
		Args:   []string{"/bin/sh"},
		Limits: cgroup.Limits{MemoryBytes: -1},
	})
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("err = %v, want a validation error naming the negative limit", err)
	}
}

// TestSeccompFilterSurvivesExec reads the kernel's own account of the build
// command's state. seccomp_linux_test.go proves what the filter would decide;
// this proves the filter is loaded, in filter mode, on the process that
// matters — the exec'd busybox, not the Go child that installed it.
func TestSeccompFilterSurvivesExec(t *testing.T) {
	out, err := sh(t, "awk '/^Seccomp:|^Seccomp_filters:|^NoNewPrivs:/ {print $1 $2}' /proc/self/status")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	// Mode 2 is SECCOMP_MODE_FILTER; exactly one filter, because a second
	// would mean the child installed it twice. The order is the kernel's, in
	// fs/proc/array.c — NoNewPrivs is printed before the seccomp fields.
	want := "NoNewPrivs:1\nSeccomp:2\nSeccomp_filters:1"
	if out != want {
		t.Fatalf("/proc/self/status = %q, want %q", out, want)
	}
}

// TestSeccompDeniesMknod is the syscall the private /dev deliberately does not
// bound: sandbox root keeps CAP_MKNOD, so until there is a filter, a build can
// make itself a block device for the host's disk. The rootfs is bind-mounted
// and shared between builds, so a failure here writes into the image every
// later build starts from — hence the cleanup on both sides.
func TestSeccompDeniesMknod(t *testing.T) {
	dir := rootfs(t)
	const probe = "/tmp/forge-seccomp-probe"
	t.Cleanup(func() {
		// The script below removes this itself, but only if the sandbox got
		// far enough to run it.
		if err := os.Remove(filepath.Join(dir, strings.TrimPrefix(probe, "/"))); err == nil {
			t.Errorf("%s was left in the rootfs image: mknod was not denied", probe)
		}
	})

	out, err := sh(t, "mknod "+probe+" c 1 3 2>/dev/null && echo made || echo denied; "+
		"test -e "+probe+" && echo present || echo absent; rm -f "+probe)
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if want := "denied\nabsent"; out != want {
		t.Fatalf("mknod inside the sandbox = %q, want %q", out, want)
	}
}

// TestSeccompDeniesMount covers the other half: a build that can mount can
// bring the host's filesystem back into a root the sandbox spent five steps
// getting rid of.
func TestSeccompDeniesMount(t *testing.T) {
	out, err := sh(t, "mount -t tmpfs none /mnt 2>/dev/null && echo mounted || echo denied")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if out != "denied" {
		t.Fatalf("mount inside the sandbox = %q, want denied", out)
	}
}

// TestAnOrdinaryBuildIsUnaffected is the other direction, and the one an
// allowlist gets wrong: a filter that denies too much passes every test above
// and breaks every real build. The script is deliberately mundane — files,
// pipes, a subshell, an archive, an in-place edit, a read of /dev/urandom —
// and `set -e` makes any step that hits a denied syscall fail the test.
func TestAnOrdinaryBuildIsUnaffected(t *testing.T) {
	const script = `set -e
mkdir -p /tmp/forge-build && cd /tmp/forge-build
for i in 1 2 3; do echo line-$i > f$i; done
cat f1 f2 f3 | sort -r | head -1
dd if=/dev/urandom of=r bs=1k count=4 2>/dev/null && echo random-read
ln -s f1 link && readlink link
chmod 600 f1
tar cf a.tar f1 f2 f3 && rm -f f1 f2 f3 && tar xf a.tar && cat f2
sed -i 's/line/LINE/' f3 && cat f3
( echo sub > bg ) &
wait
cat bg
ls f1 f2 f3
cd / && rm -rf /tmp/forge-build`

	dir := rootfs(t)
	t.Cleanup(func() { os.RemoveAll(filepath.Join(dir, "tmp/forge-build")) })

	out, err := sh(t, script)
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	want := "line-3\nrandom-read\nf1\nline-2\nLINE-3\nsub\nf1\nf2\nf3"
	if out != want {
		t.Fatalf("build output =\n%q\nwant\n%q", out, want)
	}
}

// TestSandboxRootIsAnUnprivilegedHostUid is what the user namespace is for: the
// build believes it is root and the kernel disagrees. The map is read from
// inside, where the kernel prints it as "inside outside count": the outside
// field pins that uid 0 in the sandbox is mappedBase on the host, and the
// count that host root is not among the ids the sandbox can name. `id -u`
// is the other half — a child cloned from root keeps host root unless
// Credential moves it, and host root is unmapped here, so it would print
// the overflow uid. The chown is the assertion behind mapping a range rather
// than the single id the build runs as: a package manager that chowns is
// the ordinary case, and gets EINVAL for any id outside the map.
func TestSandboxRootIsAnUnprivilegedHostUid(t *testing.T) {
	dir := rootfs(t)
	t.Cleanup(func() { os.Remove(filepath.Join(dir, "tmp/theirs")) })
	out, err := sh(t, "set -e; id -u; touch /tmp/theirs; chown 1000:1000 /tmp/theirs; "+
		"stat -c %u /tmp/theirs; cat /proc/self/uid_map")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	want := fmt.Sprintf("0 1000 0 %d %d", mappedBase, mappedCount)
	if got := strings.Join(strings.Fields(out), " "); got != want {
		t.Fatalf("uid, chown result and uid_map inside the sandbox = %q, want %q", got, want)
	}
}

// TestSetgroupsIsDeniedInTheNamespace pins the one line of this change with no
// visible effect on a build. A process that can call setgroups(2) inside the
// namespace can drop a supplementary group, and dropping a group is how a
// mapped uid gets past a directory that excludes it by negative permission.
// The kernel records the decision where the sandbox itself can read it.
func TestSetgroupsIsDeniedInTheNamespace(t *testing.T) {
	out, err := sh(t, "cat /proc/self/setgroups")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if out != "deny" {
		t.Fatalf("setgroups is %q in the sandbox's user namespace, want deny", out)
	}
}

// TestTheImageIsNeverWritten is the overlay's reason to exist. A build may
// write anywhere — /usr, an existing file in /etc, delete one, replace a
// directory — and sees its own writes; the image on the host is the same
// afterwards, byte for byte where it was edited and present where it was
// deleted.
//
// The replaced directory is the case that depends on the overlay's own
// metadata: it marks a directory that hides its lower namesake with an
// xattr, and an overlay that could not set one would list the image's
// entries through the directory a build removed and made again.
func TestTheImageIsNeverWritten(t *testing.T) {
	dir := scratchImage(t)
	hostname, err := os.ReadFile(filepath.Join(dir, "etc/hostname"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := os.ReadDir(filepath.Join(dir, "etc/apk/keys"))
	if err != nil || len(keys) == 0 {
		t.Fatalf("etc/apk/keys in the image: %d entries, %v", len(keys), err)
	}

	out, err := shIn(t, dir, `set -e
echo probe > /usr/forge-probe && cat /usr/forge-probe
echo edited >> /etc/hostname && tail -n1 /etc/hostname
rm /etc/motd && test ! -e /etc/motd && echo motd-gone
rm -r /etc/apk/keys && mkdir /etc/apk/keys && echo "keys: $(ls /etc/apk/keys | wc -l)"`)
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if out != "probe\nedited\nmotd-gone\nkeys: 0" {
		t.Fatalf("output = %q: a build cannot see its own writes", out)
	}

	if _, err := os.Stat(filepath.Join(dir, "usr/forge-probe")); !os.IsNotExist(err) {
		t.Errorf("usr/forge-probe exists in the image on the host (stat: %v): the write reached the lower layer", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "etc/hostname")); !bytes.Equal(after, hostname) {
		t.Errorf("etc/hostname in the image changed from %q to %q", hostname, after)
	}
	if _, err := os.Stat(filepath.Join(dir, "etc/motd")); err != nil {
		t.Errorf("etc/motd is gone from the image on the host: %v", err)
	}
	if after, _ := os.ReadDir(filepath.Join(dir, "etc/apk/keys")); len(after) != len(keys) {
		t.Errorf("etc/apk/keys in the image has %d entries, had %d", len(after), len(keys))
	}
}

// TestNoWritableStateOutlivesAJob is the build-to-build half: the layer is a
// tmpfs in the job's own mount namespace, so it is gone with the namespace,
// and the next job starts from the image and nothing else.
func TestNoWritableStateOutlivesAJob(t *testing.T) {
	dir := scratchImage(t)
	out, err := shIn(t, dir, "touch /usr/forge-first /tmp/forge-first && echo written")
	if err != nil {
		t.Fatalf("first job: %v (output %q)", err, out)
	}
	out, err = shIn(t, dir, "ls /usr/forge-first /tmp/forge-first 2>/dev/null; echo checked")
	if err != nil {
		t.Fatalf("second job: %v (output %q)", err, out)
	}
	if out != "checked" {
		t.Fatalf("the second job found the first job's files: %q", out)
	}
}

// TestAPackageManagerCanChangeTheInstalledSet runs the real thing over the
// overlay: apk rewrites its database, unlinks files under /usr and /etc, and
// does it all as root of a filesystem it now believes it owns. Removal rather
// than install because the sandbox has no network; it exercises the same
// paths. The image keeps the package.
func TestAPackageManagerCanChangeTheInstalledSet(t *testing.T) {
	dir := scratchImage(t)
	out, err := shIn(t, dir, "set -e; apk del --quiet libc-utils 2>/dev/null; "+
		"apk info 2>/dev/null | grep -c '^libc-utils$' || true; "+
		"test ! -e /usr/bin/getconf && echo getconf-gone")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if out != "0\ngetconf-gone" {
		t.Fatalf("output = %q, want the package gone from apk's database and its files from /usr", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "usr/bin/getconf")); err != nil {
		t.Fatalf("apk del reached the image on the host: %v", err)
	}
}

// TestTheLayerCountsAgainstTheMemoryLimit is why there is no separate size
// for it. tmpfs pages are charged to the cgroup of the process that writes
// them, so a build filling its root filesystem meets the same limit and the
// same OOM kill as one filling its heap.
func TestTheLayerCountsAgainstTheMemoryLimit(t *testing.T) {
	out, err := runSpec(t, Spec{
		Rootfs:     scratchImage(t),
		CgroupRoot: testCgroupRoot(t),
		Limits:     cgroup.Limits{MemoryBytes: 16 << 20},
		Args:       []string{"/bin/sh", "-c", "dd if=/dev/zero of=/usr/forge-blob bs=1M count=64 2>/dev/null && echo survived"},
	})
	if !errors.Is(err, ErrOOMKilled) {
		t.Fatalf("err = %v, want it to wrap ErrOOMKilled (output %q)", err, out)
	}
	if strings.Contains(out, "survived") {
		t.Fatalf("64MiB were written into the layer under a 16MiB limit: %q", out)
	}
}

// TestABuildCannotCreateANamespace is the installed half of bounding clone:
// the policy test in seccomp_linux_test.go runs the filter in Go, this runs
// it in the kernel. The fork first, so that a refusal of the namespace is
// known to be the flags and not the probe.
func TestABuildCannotCreateANamespace(t *testing.T) {
	if out := runProbe(t, "clone-fork"); out != "clone: ok" {
		t.Fatalf("a plain fork inside the sandbox: %q, want it to work", out)
	}
	if out := runProbe(t, "clone-newuser"); out != "clone: operation not permitted" {
		t.Fatalf("clone(CLONE_NEWUSER) inside the sandbox: %q, want EPERM", out)
	}
}

// TestTheBoundingSetIsTheKeepList reads the sets the kernel reports for the
// build after execve. The mask is written out rather than computed from
// keptCapabilities, so that a capability added to the keep list is a
// failing test and not a passing one. CAP_SYS_ADMIN is also checked by name,
// because it is the one the whole exercise is for.
func TestTheBoundingSetIsTheKeepList(t *testing.T) {
	// CHOWN, DAC_OVERRIDE, FOWNER, FSETID, KILL, SETGID, SETUID,
	// NET_BIND_SERVICE, SETFCAP.
	const want = "00000000800004fb"
	const sysAdmin = 1 << 21
	out, err := sh(t, "grep -E '^Cap(Inh|Prm|Eff|Bnd|Amb):' /proc/self/status")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	sets := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			sets[k] = strings.TrimSpace(v)
		}
	}
	for _, set := range []string{"CapPrm", "CapEff", "CapBnd"} {
		if sets[set] != want {
			t.Errorf("%s = %s, want %s", set, sets[set], want)
		}
		if v, _ := strconv.ParseUint(sets[set], 16, 64); v&sysAdmin != 0 {
			t.Errorf("%s holds CAP_SYS_ADMIN", set)
		}
	}
	for _, set := range []string{"CapInh", "CapAmb"} {
		if sets[set] != "0000000000000000" {
			t.Errorf("%s = %s, want empty: the exec would confer it", set, sets[set])
		}
	}
}

// TestADroppedCapabilityIsGone is the behavioural half. A raw socket needs
// CAP_NET_RAW in the user namespace that owns the network namespace, which
// is this sandbox's own — so root here would ordinarily have it, and the
// bounding set is the only reason it does not. Chosen over the obvious
// probes because the kernel checks most of those against the initial
// namespace, where this root never had anything to lose. The chown in
// TestSandboxRootIsAnUnprivilegedHostUid is the other direction — a kept
// capability still works.
func TestADroppedCapabilityIsGone(t *testing.T) {
	if out := runProbe(t, "raw-socket"); out != "socket: operation not permitted" {
		t.Fatalf("socket(AF_INET, SOCK_RAW) inside the sandbox: %q, want EPERM: the build still holds CAP_NET_RAW", out)
	}
}
