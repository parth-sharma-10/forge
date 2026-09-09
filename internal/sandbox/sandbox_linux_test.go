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
	"strings"
	"testing"
	"time"
)

// TestMain gives the test binary the same dual identity every Forge binary
// has: Run re-executes /proc/self/exe, which here is the test binary itself,
// so it must take the child path instead of running the suite again.
func TestMain(m *testing.M) {
	MaybeChild()
	os.Exit(m.Run())
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

func runArgs(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), Spec{
		Rootfs:   rootfs(t),
		Hostname: "forge-test",
		Args:     args,
		Stdout:   &out,
		Stderr:   &out,
	})
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

	out, err := sh(t, "test -e "+marker.Name()+" && echo VISIBLE || echo HIDDEN; test -e /"+oldRootDir+" && echo OLDROOT || echo DETACHED")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if want := "HIDDEN\nDETACHED"; out != want {
		t.Fatalf("got %q, want %q: the host root is still reachable from inside", out, want)
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
