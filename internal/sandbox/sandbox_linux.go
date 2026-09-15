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

// namespaces is the isolation applied at clone time. All six are unshared in
// the clone itself rather than with unshare(2) afterwards, because CLONE_NEWPID
// only takes effect for children of the calling process: a process cannot move
// itself into a new PID namespace, so the re-exec is what puts the build
// command at PID 1.
//
// The user namespace is what gives the other five their teeth. Each of them is
// owned by it, so the child holds CAP_SYS_ADMIN *in them* — enough to mount and
// pivot_root — while holding nothing on the host side of the same boundary.
//
// Not here yet: CLONE_NEWCGROUP. The seccomp filter is not a clone flag: it is
// installed by the child on itself, last, once the setup syscalls it denies are
// no longer needed. See seccomp_linux.go.
const namespaces = syscall.CLONE_NEWUSER | // user:    sandbox root is an unprivileged host uid
	syscall.CLONE_NEWNS | //  mount:   its own filesystem tree
	syscall.CLONE_NEWUTS | // uts:     its own hostname
	syscall.CLONE_NEWIPC | // ipc:     no shared memory or message queues
	syscall.CLONE_NEWPID | // pid:     the command is PID 1 and sees no host process
	syscall.CLONE_NEWNET //  network: no interfaces at all, not even routed ones

// The sandbox's id map: uid 0 inside is uid mappedBase outside, and the 65536
// ids the sandbox can name run from there. A range rather than the single id
// the sandbox actually runs as, because a build that chowns — every package
// manager does — gets EINVAL for any id the map does not cover.
//
// mappedBase is past every id a distribution assigns (systemd stops at 65535)
// and clear of 100000, where /etc/subuid starts handing 65536-id ranges to real
// users for rootless podman and docker. Colliding with one of those is not a
// hole in the sandbox; it makes a build's files show up owned by somebody.
//
// ponytail: one map for every job on the worker, so this is a boundary between
// a build and the host, not between two builds. Per-job ranges when the overlay
// rootfs lands and two builds can write to the same place.
const (
	mappedBase  = 524288
	mappedCount = 65536
)

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
		// Sandbox root is host uid mappedBase: the build owns nothing on the
		// host, and CAP_SYS_ADMIN inside its namespaces is not CAP_SYS_ADMIN
		// against the kernel's own. Credential is what moves the process to
		// that uid — the map alone is decoration, since a child cloned from
		// root keeps the host root it was cloned with.
		//
		// Dropping from root to an unmapped uid normally clears every
		// capability, which would leave the child unable to mount anything.
		// It does not here: the kernel compares against root *of the process's
		// own user namespace*, and mappedBase is exactly that — this reads as
		// a move to root rather than away from it.
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: mappedBase, Size: mappedCount}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: mappedBase, Size: mappedCount}},
		// Left denied, which is the field's whole reason for existing: a
		// process that can call setgroups(2) inside the namespace can drop a
		// group it was excluded by, and no build needs to change its groups.
		GidMappingsEnableSetgroups: false,
		Credential:                 &syscall.Credential{Uid: 0, Gid: 0},
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

// mountProc gives the sandbox the process table of its own PID namespace. It
// runs before pivot_root, and has to: mounting a fresh proc from inside a user
// namespace is allowed only while a fully visible proc is already mounted in
// the mount namespace, and detaching the old root takes the host's away. Done
// in the other order the mount fails with EPERM, from a process holding
// CAP_SYS_ADMIN, for a reason that has nothing to do with capabilities.
//
// The instance is the new PID namespace's view either way — that is fixed at
// mount time by the namespace the mounting process is in, not by where the
// mount point sits.
func mountProc(rootfs string) error {
	proc := filepath.Join(rootfs, "proc")
	if err := os.MkdirAll(proc, 0o555); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	const procFlags = syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC
	if err := syscall.Mount("proc", proc, "proc", procFlags, ""); err != nil {
		return fmt.Errorf("sandbox: mount %s: %w", proc, err)
	}
	return nil
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
// above. It runs before pivot_root, on the rootfs where it is still a path on
// the host, because the host's /dev has to be readable to bind from.
//
// The nodes are bind-mounted rather than made with mknod: a process in a user
// namespace cannot create a device node at all — the kernel wants CAP_MKNOD in
// the *initial* namespace, which is the one privilege the sandbox has given up.
// So the numbers below stop being arguments to mknod and become an assertion
// about the host instead: /dev/null is bound in only once it is confirmed to be
// character device 1:3, and a host whose /dev/null is something else fails the
// build rather than lending it to the sandbox.
//
// What does not change is that the host's /dev is never mounted in wholesale: a
// sandbox that can reach /dev/sda or /dev/mem is not a sandbox. The set is
// exactly the six listed above.
//
// The tmpfs underneath is what keeps the nodes out of the rootfs image. Without
// it the image's empty dev/ directory is writable, so a build redirecting to
// /dev/null quietly creates a regular file of that name — in a directory the
// next build on this worker will inherit.
//
// ponytail: no /dev/shm, /dev/ptmx or devpts. Nothing needs them until a build
// wants a pty or POSIX shared memory; add them when one does.
func mountDev(rootfs string) error {
	dev := filepath.Join(rootfs, "dev")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	// MS_NODEV is safe here since the nodes became bind mounts: each carries
	// the host mount's own flags, so the tmpfs's flags never reach them. It
	// only bites a device node created *on* the tmpfs, which nothing can do.
	// 64k is room for the mount points and the symlinks and nothing else.
	const devFlags = syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV
	if err := syscall.Mount("tmpfs", dev, "tmpfs", devFlags, "mode=0755,size=64k"); err != nil {
		return fmt.Errorf("sandbox: mount %s: %w", dev, err)
	}
	for _, n := range devNodes {
		src := "/dev/" + n.name
		var st syscall.Stat_t
		if err := syscall.Stat(src, &st); err != nil {
			return fmt.Errorf("sandbox: host %s: %w", src, err)
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFCHR || st.Rdev != uint64(mkdev(n.major, n.minor)) {
			return fmt.Errorf("sandbox: host %s is not character device %d:%d", src, n.major, n.minor)
		}
		// The mount point's own mode never shows: a bind mount presents the
		// source node, which is where /dev/null's 0666 comes from.
		dst := filepath.Join(dev, n.name)
		if err := os.WriteFile(dst, nil, 0o600); err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
		if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("sandbox: bind %s: %w", dst, err)
		}
	}
	for _, l := range devLinks {
		if err := os.Symlink(l[1], filepath.Join(dev, l[0])); err != nil {
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
//  3. mount /proc and /dev onto the rootfs while it is still a path on the
//     host, because after step 4 neither can be done at all — see mountProc
//     and mountDev for the two different reasons;
//  4. pivot_root, then detach the old root, so no path to the host remains
//     (chroot alone would leave one — an open directory fd escapes it);
//  5. install the seccomp filter last of all, since every step above uses
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

	if err := mountProc(rootfs); err != nil {
		return err
	}
	if err := mountDev(rootfs); err != nil {
		return err
	}

	// pivot_root(".", ".") — the new root doubles as the place the old one is
	// parked, stacked on top of it, so nothing has to be created inside the
	// rootfs to hold it. That matters now that the image is owned by a uid
	// this namespace does not map: a directory could not be made there, and
	// the empty one left behind could not be removed.
	if err := syscall.Chdir(rootfs); err != nil {
		return fmt.Errorf("sandbox: chdir %s: %w", rootfs, err)
	}
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("sandbox: pivot_root %s: %w", rootfs, err)
	}
	// "." still resolves through the old root stacked over the new one, so
	// this unmounts the host tree rather than anything of the sandbox's.
	// MNT_DETACH rather than a plain unmount: the host tree is busy — this
	// process was started from it — and a lazy unmount is what makes it
	// unreachable by name regardless.
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("sandbox: detach old root: %w", err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("sandbox: chdir after pivot_root: %w", err)
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
