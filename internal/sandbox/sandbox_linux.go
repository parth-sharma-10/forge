//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/parth-sharma-10/forge/internal/cgroup"
)

// namespaces is the isolation applied at clone time. All five are unshared in
// the clone itself rather than with unshare(2) afterwards, because CLONE_NEWPID
// only takes effect for children of the calling process: a process cannot move
// itself into a new PID namespace, so the re-exec is what puts the build
// command at PID 1.
//
// Not here yet: CLONE_NEWUSER (the sandbox runs as real root, so a container
// root is host root — Phase 1), CLONE_NEWCGROUP, and seccomp.
const namespaces = syscall.CLONE_NEWNS | // mount:   its own filesystem tree
	syscall.CLONE_NEWUTS | // uts:     its own hostname
	syscall.CLONE_NEWIPC | // ipc:     no shared memory or message queues
	syscall.CLONE_NEWPID | // pid:     the command is PID 1 and sees no host process
	syscall.CLONE_NEWNET //  network: no interfaces at all, not even routed ones

// sandboxPATH is the search path used inside the new root. The host's PATH is
// meaningless there and would resolve to nothing.
const sandboxPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Run executes spec.Args inside a fresh sandbox and waits for it to exit.
//
// The returned error is an *exec.ExitError carrying the command's own exit
// status when the command ran and failed; SetupExitCode means the sandbox
// itself could not be built, with the reason on spec.Stderr.
//
// Cancelling ctx kills the child, which is PID 1 of the sandbox's PID
// namespace — the kernel then kills everything else in that namespace, so a
// build cannot outlive its context by forking away.
func Run(ctx context.Context, spec Spec) error {
	if err := spec.validate(); err != nil {
		return err
	}

	group, err := cgroup.Create(spec.CgroupRoot, spec.Limits)
	if err != nil {
		return err
	}
	defer group.Close()

	cmd := exec.CommandContext(ctx, "/proc/self/exe", append([]string{ChildArg}, spec.Args...)...)
	cmd.Env = []string{
		envRootfs + "=" + spec.Rootfs,
		envHostname + "=" + spec.Hostname,
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = spec.Stdin, spec.Stdout, spec.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: namespaces,
		// A worker that dies mid-build must not leave the build running: the
		// sandbox has no supervisor of its own to notice.
		Pdeathsig: syscall.SIGKILL,
		// The kernel puts the child in the cgroup as part of the clone, so the
		// limits are in force before the child's first instruction. Setting
		// them by writing cgroup.procs after Start would leave a window in
		// which the build runs uncapped.
		UseCgroupFD: true,
		CgroupFD:    group.FD(),
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("sandbox: start child (needs CAP_SYS_ADMIN — run as root): %w", err)
	}
	runErr := cmd.Wait()

	// Asked after the wait and before Close removes the cgroup: this is the
	// only moment the counter is both final and still readable.
	if runErr != nil {
		if oom, err := group.OOMKilled(); err != nil {
			return fmt.Errorf("%w (could not check for an OOM kill: %w)", runErr, err)
		} else if oom {
			return fmt.Errorf("%w: %w", ErrOOMKilled, runErr)
		}
	}
	return runErr
}

// MaybeChild applies the sandbox to this process and execs the build command
// over it, if this process is the re-executed child. Otherwise it returns
// immediately. It must be called before flag parsing, since the child's argv
// is Forge's, not the caller's.
func MaybeChild() {
	if len(os.Args) < 2 || os.Args[1] != ChildArg {
		return
	}
	// child returns only on failure; success is an exec, which never returns.
	err := child()
	fmt.Fprintf(os.Stderr, "%v\n", err)
	os.Exit(SetupExitCode)
}

// child runs inside the new namespaces. Ordering is the whole substance of it:
//
//  1. make every mount private, or step 2 propagates back to the host and
//     pivot_root refuses a shared new root;
//  2. bind the rootfs onto itself, or pivot_root refuses a non-mount-point;
//  3. pivot_root, then detach the old root, so no path to the host remains
//     (chroot alone would leave one — an open directory fd escapes it);
//  4. only then mount /proc, so it is the new PID namespace's view rather
//     than the host's process table.
func child() error {
	rootfs := os.Getenv(envRootfs)
	if rootfs == "" {
		return fmt.Errorf("sandbox: child: %s is not set", envRootfs)
	}
	argv := os.Args[2:]
	if len(argv) == 0 {
		return fmt.Errorf("sandbox: child: no command to run")
	}

	if hostname := os.Getenv(envHostname); hostname != "" {
		if err := syscall.Sethostname([]byte(hostname)); err != nil {
			return fmt.Errorf("sandbox: sethostname %q: %w", hostname, err)
		}
	}

	if err := syscall.Mount("none", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("sandbox: make mounts private: %w", err)
	}
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("sandbox: bind rootfs %s: %w", rootfs, err)
	}

	oldRoot := filepath.Join(rootfs, oldRootDir)
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	if err := syscall.PivotRoot(rootfs, oldRoot); err != nil {
		return fmt.Errorf("sandbox: pivot_root %s: %w", rootfs, err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("sandbox: chdir after pivot_root: %w", err)
	}

	// MNT_DETACH rather than a plain unmount: the host tree is busy — this
	// process was started from it — and a lazy unmount is what makes it
	// unreachable by name regardless.
	stale := "/" + oldRootDir
	if err := syscall.Unmount(stale, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("sandbox: detach old root: %w", err)
	}
	if err := os.Remove(stale); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	if err := os.MkdirAll("/proc", 0o555); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	const procFlags = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC
	if err := syscall.Mount("proc", "/proc", "proc", procFlags, ""); err != nil {
		return fmt.Errorf("sandbox: mount /proc: %w", err)
	}

	// ponytail: /dev is whatever the image ships, usually nothing, so a build
	// that writes to /dev/null fails. Needs a tmpfs with the standard nodes
	// bind-mounted in — that lands with the cgroup and device-cgroup work.

	if err := os.Setenv("PATH", sandboxPATH); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	return syscall.Exec(path, argv, []string{"PATH=" + sandboxPATH, "HOME=/root"})
}
