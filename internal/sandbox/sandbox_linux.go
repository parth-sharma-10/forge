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
// root is host root — Phase 1) and CLONE_NEWCGROUP. The seccomp filter is not
// a clone flag: it is installed by the child on itself, last, once the
// setup syscalls it denies are no longer needed. See seccomp_linux.go.
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

// devNodes are the character devices a build expects to find. The major and
// minor numbers are the kernel's, fixed in Documentation/admin-guide/devices.txt
// — they are not chosen here and must not be invented.
var devNodes = []struct {
	name         string
	major, minor uint32
}{
	{"null", 1, 3},
	{"zero", 1, 5},
	{"full", 1, 7},
	{"random", 1, 8},
	{"urandom", 1, 9},
	// Opening this with no controlling terminal fails with ENXIO, which is
	// what a tool probing for a tty expects to see. Its absence is not.
	{"tty", 5, 0},
}

// devLinks resolve through the sandbox's own /proc, so they name its file
// descriptors rather than the host's.
var devLinks = [][2]string{
	{"fd", "/proc/self/fd"},
	{"stdin", "/proc/self/fd/0"},
	{"stdout", "/proc/self/fd/1"},
	{"stderr", "/proc/self/fd/2"},
}

// mountDev gives the sandbox a private /dev holding nothing but the nodes
// above.
//
// The host's /dev is never consulted, let alone bind-mounted in: a sandbox
// that can reach /dev/sda or /dev/mem is not a sandbox. These nodes are made
// from their kernel numbers on a fresh tmpfs, so the set is exactly what is
// listed here and nothing depends on what the host happens to have.
//
// The tmpfs is also what keeps the nodes out of the rootfs image. Without it
// the image's empty dev/ directory is writable, so a build redirecting to
// /dev/null quietly creates a regular file of that name — in a directory the
// next build on this worker will inherit.
//
// ponytail: no /dev/shm, /dev/ptmx or devpts. Nothing needs them until a build
// wants a pty or POSIX shared memory; add them when one does.
func mountDev() error {
	// mknod's mode is masked by the umask, which would leave /dev/null at
	// 0644 and unwritable by any build that is not root.
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	if err := os.MkdirAll("/dev", 0o755); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	// Not MS_NODEV, which would make every node below inert — that flag is
	// for filesystems that have no business carrying devices, and this one's
	// entire purpose is to carry six of them. 64k is room for the nodes and
	// the symlinks and nothing else.
	const devFlags = syscall.MS_NOSUID | syscall.MS_NOEXEC
	if err := syscall.Mount("tmpfs", "/dev", "tmpfs", devFlags, "mode=0755,size=64k"); err != nil {
		return fmt.Errorf("sandbox: mount /dev: %w", err)
	}
	for _, n := range devNodes {
		path := "/dev/" + n.name
		if err := syscall.Mknod(path, syscall.S_IFCHR|0o666, mkdev(n.major, n.minor)); err != nil {
			return fmt.Errorf("sandbox: mknod %s (c %d:%d): %w", path, n.major, n.minor, err)
		}
	}
	for _, l := range devLinks {
		if err := os.Symlink(l[1], "/dev/"+l[0]); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
	}
	return nil
}

// mkdev packs a device number the way mknod(2) takes it — Linux's
// new_encode_dev, which splits the minor around the major rather than putting
// the two side by side.
func mkdev(major, minor uint32) int {
	return int((minor & 0xff) | (major << 8) | ((minor &^ 0xff) << 12))
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
//     than the host's process table;
//  5. mount a tmpfs /dev and make the standard nodes on it, so a build that
//     opens /dev/null does not instead create a file of that name in the
//     shared rootfs image;
//  6. install the seccomp filter last of all, since every step above uses
//     syscalls it denies — and after the command has been resolved, because
//     that walks PATH and the filter has no reason to be in force for it.
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

	if err := mountDev(); err != nil {
		return err
	}

	if err := os.Setenv("PATH", sandboxPATH); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	if err := applySeccomp(); err != nil {
		return err
	}
	return syscall.Exec(path, argv, []string{"PATH=" + sandboxPATH, "HOME=/root"})
}
