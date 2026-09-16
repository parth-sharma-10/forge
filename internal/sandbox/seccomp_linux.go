//go:build linux

package sandbox

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// A seccomp filter is a classic BPF program the kernel runs on every syscall,
// against a struct seccomp_data holding the syscall number, the architecture
// it was issued under, and its six arguments. This one is an allowlist: it
// compares the number against everything a build is expected to need and
// denies whatever is left, so a syscall added by a future kernel is denied by
// having never been listed rather than by someone remembering to block it.
//
// Written out as instructions rather than built with libseccomp: the program
// below is under twenty lines of encoding, and libseccomp is cgo, which would
// put a C toolchain and a shared library between Forge and a static binary.
//
// One syscall is filtered on an argument as well as its number: clone, whose
// flags decide whether the call is a fork or a new set of namespaces. Every
// other decision is by number alone.
const (
	// BPF instruction encodings, from linux/bpf_common.h.
	bpfLoadWordAbs = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJumpEqual   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJumpSet     = 0x45 // BPF_JMP | BPF_JSET | BPF_K: jump if (A & K) != 0
	bpfReturn      = 0x06 // BPF_RET | BPF_K

	// Byte offsets into struct seccomp_data. args[0] is a u64 at 16; classic
	// BPF loads 32 bits, and on the little-endian architectures Forge builds
	// for the low half is at the lower address.
	offsetNr   = 0
	offsetArch = 4
	offsetArg0 = 16

	// Filter results, from linux/seccomp.h.
	retAllow       = 0x7fff0000 // SECCOMP_RET_ALLOW
	retKillProcess = 0x80000000 // SECCOMP_RET_KILL_PROCESS
	retErrno       = 0x00050000 // SECCOMP_RET_ERRNO, or'd with the errno to return

	// cloneNamespaceFlags are the CLONE_NEW* bits: every one of them asks the
	// kernel for a namespace the build would own. The legacy clone reads its
	// flags from the low 32 bits of the first argument and ignores the rest,
	// so a mask over the low word is a mask over everything the kernel sees.
	// CLONE_NEWTIME is not here because it is not here for clone either: its
	// bit is inside CSIGNAL, and only clone3 can pass it — which is denied.
	cloneNamespaceFlags = syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS | syscall.CLONE_NEWIPC |
		syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNET | syscall.CLONE_NEWCGROUP

	// maxFilterLen is BPF_MAXINSNS: the kernel rejects a longer program.
	maxFilterLen = 4096

	prSetNoNewPrivs = 38 // PR_SET_NO_NEW_PRIVS

	seccompSetModeFilter = 1 // SECCOMP_SET_MODE_FILTER
	seccompFilterTSync   = 1 // SECCOMP_FILTER_FLAG_TSYNC
)

// denyAction is what an unlisted syscall returns: ENOSYS, not EPERM.
//
// ENOSYS is the one errno a C library treats as "this kernel is older than I
// am, use the fallback" — glibc issues clone3 and drops back to clone on
// ENOSYS, statx and drops back to fstatat, and so on. EPERM is not a fallback
// signal, so an allowlist that returns it breaks every program built against a
// libc newer than the list, in a way that surfaces as an unexplained failure
// deep inside a build.
//
// The cost is that a denial is indistinguishable from an unimplemented syscall
// when reading a build log. That is the right trade for a build sandbox and
// the wrong one for an audited boundary; SECCOMP_RET_KILL_PROCESS is what
// makes a denial loud, and it is a one-constant change here.
const denyAction = retErrno | uint32(syscall.ENOSYS)

// cloneDenyAction is what clone returns when asked for a namespace: EPERM,
// not ENOSYS. The syscall exists and the build may use it; this particular
// use is refused, which is what the kernel itself says to a process without
// the privilege to create namespaces. ENOSYS would tell a C library to try
// an older way of doing the same thing, and there is none.
const cloneDenyAction = retErrno | uint32(syscall.EPERM)

// seccompFilter builds the BPF program for the current architecture.
//
// Every jump offset is small and local — each comparison either falls through
// to its own return or steps over it to the next comparison. The alternative,
// one comparison per syscall jumping to a shared return at the end, is half
// the instructions and computes an offset that silently overflows the uint8
// jump field once the allowlist passes 255 entries.
//
// clone is the one entry with a second test. Its block reads the flags into
// the accumulator, which is why it must end in a return on both paths: a
// fall-through would compare the flags, not the number, against the rest of
// the list. A syscall that is not clone jumps over the whole block with the
// number still loaded.
func seccompFilter() []syscall.SockFilter {
	allowed := allowedSyscalls()
	f := make([]syscall.SockFilter, 0, 2*len(allowed)+8)

	// Reject a syscall issued under any other ABI before reading its number:
	// under x32 or i386 compat the same numbers name entirely different
	// syscalls, so the comparisons below would be checking the wrong table.
	// That is not something a build does by accident.
	f = append(f,
		syscall.SockFilter{Code: bpfLoadWordAbs, K: offsetArch},
		syscall.SockFilter{Code: bpfJumpEqual, Jt: 1, K: auditArch},
		syscall.SockFilter{Code: bpfReturn, K: retKillProcess},
		syscall.SockFilter{Code: bpfLoadWordAbs, K: offsetNr},
	)
	for _, nr := range allowed {
		if nr == syscall.SYS_CLONE {
			f = append(f,
				syscall.SockFilter{Code: bpfJumpEqual, Jf: 4, K: uint32(nr)},
				syscall.SockFilter{Code: bpfLoadWordAbs, K: offsetArg0},
				syscall.SockFilter{Code: bpfJumpSet, Jt: 1, K: cloneNamespaceFlags},
				syscall.SockFilter{Code: bpfReturn, K: retAllow},
				syscall.SockFilter{Code: bpfReturn, K: cloneDenyAction},
			)
			continue
		}
		f = append(f,
			syscall.SockFilter{Code: bpfJumpEqual, Jf: 1, K: uint32(nr)},
			syscall.SockFilter{Code: bpfReturn, K: retAllow},
		)
	}
	return append(f, syscall.SockFilter{Code: bpfReturn, K: denyAction})
}

// applySeccomp installs the filter on this process and every thread of it.
//
// It is called last in the child, after the mounts, because building the
// sandbox needs the syscalls the sandbox must not have — mount, pivot_root
// and mknod are exactly what a build must not reach and exactly what putting
// a build behind this boundary requires.
//
// The filter is inherited across the execve that follows and cannot be
// removed, which is the property the whole thing rests on.
func applySeccomp() error {
	f := seccompFilter()
	if len(f) > maxFilterLen {
		return fmt.Errorf("sandbox: seccomp filter is %d instructions, kernel limit is %d", len(f), maxFilterLen)
	}
	prog := syscall.SockFprog{Len: uint16(len(f)), Filter: &f[0]}

	// no_new_privs is the kernel's precondition for an unprivileged
	// seccomp(2), and this child is root today, so it is not strictly needed
	// yet. It becomes load-bearing the moment the user namespace lands and
	// sandbox root stops being host root; setting it now costs one syscall
	// and means that change does not have to remember this one.
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("sandbox: set no_new_privs: %w", errno)
	}

	// TSYNC, rather than filtering this thread alone: Go can move a goroutine
	// between OS threads at any call, so the thread that installs the filter
	// is not guaranteed to be the thread that reaches the execve below.
	_, _, errno := syscall.RawSyscall(sysSeccomp, seccompSetModeFilter, seccompFilterTSync,
		uintptr(unsafe.Pointer(&prog)))
	runtime.KeepAlive(f)
	if errno != 0 {
		return fmt.Errorf("sandbox: install seccomp filter (%d instructions): %w", len(f), errno)
	}
	return nil
}

// allowedSyscalls is the policy: what an ordinary build needs, and nothing
// more. The numbers below are the ones Go's syscall package defines on every
// architecture Forge builds for; the rest, and the architecture's own audit
// token, are in the per-architecture files beside this one, so an unsupported
// architecture is a compile error rather than a filter built from the wrong
// numbers.
//
// Absent by design, and therefore denied: mount, umount2, pivot_root, chroot,
// mknod, ptrace, process_vm_readv, bpf, perf_event_open, init_module,
// kexec_load, setns, unshare, keyctl, userfaultfd, open_by_handle_at, iopl,
// syslog, reboot, swapon, settimeofday and the rest of the kernel-facing
// surface. clone3 too: its flags live in a struct the filter cannot read, so
// it is denied by number and glibc falls back to clone, where they can be.
func allowedSyscalls() []int {
	return append([]int{
		// Process lifecycle and identity.
		syscall.SYS_EXECVE,
		syscall.SYS_EXIT,
		syscall.SYS_EXIT_GROUP,
		syscall.SYS_CLONE,
		syscall.SYS_WAIT4,
		syscall.SYS_WAITID,
		syscall.SYS_KILL,
		syscall.SYS_TKILL,
		syscall.SYS_TGKILL,
		syscall.SYS_GETPID,
		syscall.SYS_GETPPID,
		syscall.SYS_GETTID,
		syscall.SYS_GETUID,
		syscall.SYS_GETEUID,
		syscall.SYS_GETGID,
		syscall.SYS_GETEGID,
		syscall.SYS_SETUID,
		syscall.SYS_SETGID,
		syscall.SYS_SETRESUID,
		syscall.SYS_SETRESGID,
		syscall.SYS_GETRESUID,
		syscall.SYS_GETRESGID,
		syscall.SYS_SETPGID,
		syscall.SYS_GETPGID,
		syscall.SYS_SETSID,
		syscall.SYS_GETSID,
		syscall.SYS_GETGROUPS,
		syscall.SYS_SETGROUPS,
		syscall.SYS_CAPGET,
		syscall.SYS_PRCTL,
		syscall.SYS_SET_TID_ADDRESS,
		syscall.SYS_SET_ROBUST_LIST,
		syscall.SYS_GET_ROBUST_LIST,

		// Scheduling and limits.
		syscall.SYS_SCHED_YIELD,
		syscall.SYS_SCHED_GETAFFINITY,
		syscall.SYS_SCHED_SETAFFINITY,
		syscall.SYS_SCHED_GETPARAM,
		syscall.SYS_SCHED_SETPARAM,
		syscall.SYS_SCHED_GETSCHEDULER,
		syscall.SYS_SCHED_SETSCHEDULER,
		syscall.SYS_SCHED_GET_PRIORITY_MAX,
		syscall.SYS_SCHED_GET_PRIORITY_MIN,
		syscall.SYS_SCHED_RR_GET_INTERVAL,
		syscall.SYS_GETPRIORITY,
		syscall.SYS_SETPRIORITY,
		syscall.SYS_GETRLIMIT,
		syscall.SYS_SETRLIMIT,
		syscall.SYS_PRLIMIT64,
		syscall.SYS_GETRUSAGE,
		syscall.SYS_TIMES,
		syscall.SYS_UNAME,
		syscall.SYS_SYSINFO,

		// Signals.
		syscall.SYS_RESTART_SYSCALL,
		syscall.SYS_RT_SIGRETURN,
		syscall.SYS_RT_SIGACTION,
		syscall.SYS_RT_SIGPROCMASK,
		syscall.SYS_RT_SIGPENDING,
		syscall.SYS_RT_SIGSUSPEND,
		syscall.SYS_RT_SIGTIMEDWAIT,
		syscall.SYS_RT_SIGQUEUEINFO,
		syscall.SYS_RT_TGSIGQUEUEINFO,
		syscall.SYS_SIGALTSTACK,

		// Memory.
		syscall.SYS_MMAP,
		syscall.SYS_MUNMAP,
		syscall.SYS_MREMAP,
		syscall.SYS_MPROTECT,
		syscall.SYS_BRK,
		syscall.SYS_MADVISE,
		syscall.SYS_MSYNC,
		syscall.SYS_MLOCK,
		syscall.SYS_MUNLOCK,
		syscall.SYS_MLOCKALL,
		syscall.SYS_MUNLOCKALL,

		// Files and directories.
		syscall.SYS_READ,
		syscall.SYS_WRITE,
		syscall.SYS_READV,
		syscall.SYS_WRITEV,
		syscall.SYS_PREAD64,
		syscall.SYS_PWRITE64,
		syscall.SYS_PREADV,
		syscall.SYS_PWRITEV,
		syscall.SYS_OPENAT,
		syscall.SYS_CLOSE,
		syscall.SYS_FSTAT,
		syscall.SYS_LSEEK,
		syscall.SYS_FACCESSAT,
		syscall.SYS_DUP,
		syscall.SYS_DUP3,
		syscall.SYS_PIPE2,
		syscall.SYS_FCNTL,
		syscall.SYS_IOCTL,
		syscall.SYS_GETDENTS64,
		syscall.SYS_GETCWD,
		syscall.SYS_CHDIR,
		syscall.SYS_FCHDIR,
		syscall.SYS_RENAMEAT,
		syscall.SYS_MKDIRAT,
		syscall.SYS_UNLINKAT,
		syscall.SYS_SYMLINKAT,
		syscall.SYS_LINKAT,
		syscall.SYS_READLINKAT,
		syscall.SYS_FCHMOD,
		syscall.SYS_FCHMODAT,
		syscall.SYS_FCHOWN,
		syscall.SYS_FCHOWNAT,
		syscall.SYS_UMASK,
		syscall.SYS_TRUNCATE,
		syscall.SYS_FTRUNCATE,
		syscall.SYS_FALLOCATE,
		syscall.SYS_FSYNC,
		syscall.SYS_FDATASYNC,
		syscall.SYS_SYNC,
		syscall.SYS_UTIMENSAT,
		syscall.SYS_STATFS,
		syscall.SYS_FSTATFS,
		syscall.SYS_FLOCK,
		syscall.SYS_SPLICE,
		syscall.SYS_TEE,
		syscall.SYS_SENDFILE,
		syscall.SYS_FADVISE64,
		syscall.SYS_READAHEAD,

		// Waiting: polls, timers, futexes and clocks.
		syscall.SYS_EPOLL_CREATE1,
		syscall.SYS_EPOLL_CTL,
		syscall.SYS_EPOLL_PWAIT,
		syscall.SYS_EVENTFD2,
		syscall.SYS_INOTIFY_INIT1,
		syscall.SYS_INOTIFY_ADD_WATCH,
		syscall.SYS_INOTIFY_RM_WATCH,
		syscall.SYS_TIMERFD_CREATE,
		syscall.SYS_TIMERFD_SETTIME,
		syscall.SYS_TIMERFD_GETTIME,
		syscall.SYS_SIGNALFD4,
		syscall.SYS_PPOLL,
		syscall.SYS_PSELECT6,
		syscall.SYS_FUTEX,
		syscall.SYS_NANOSLEEP,
		syscall.SYS_CLOCK_NANOSLEEP,
		syscall.SYS_CLOCK_GETTIME,
		syscall.SYS_CLOCK_GETRES,
		syscall.SYS_GETTIMEOFDAY,
		syscall.SYS_SETITIMER,
		syscall.SYS_GETITIMER,

		// Sockets. The network namespace is empty, so these reach nothing but
		// AF_UNIX and a loopback that is down — the isolation there is the
		// namespace's, not this filter's. They are listed because build tools
		// open unix sockets between their own processes.
		syscall.SYS_SOCKET,
		syscall.SYS_SOCKETPAIR,
		syscall.SYS_BIND,
		syscall.SYS_CONNECT,
		syscall.SYS_LISTEN,
		syscall.SYS_ACCEPT4,
		syscall.SYS_GETSOCKNAME,
		syscall.SYS_GETPEERNAME,
		syscall.SYS_SENDTO,
		syscall.SYS_RECVFROM,
		syscall.SYS_SENDMSG,
		syscall.SYS_RECVMSG,
		syscall.SYS_SHUTDOWN,
		syscall.SYS_SETSOCKOPT,
		syscall.SYS_GETSOCKOPT,
	}, archSyscalls...)
}
