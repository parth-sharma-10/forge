//go:build linux && amd64

package sandbox

import "syscall"

// auditArch is AUDIT_ARCH_X86_64: EM_X86_64, plus the 64-bit and
// little-endian bits the audit subsystem tags an architecture with. The
// filter compares seccomp_data.arch against it so that a syscall issued under
// the x32 or i386 ABI — where these numbers name different syscalls — is
// killed rather than matched against the wrong table.
const auditArch = 0xc000003e

// sysClone3 is clone3(2), the same number on every architecture that has it.
// Named so the tests can assert it is denied; it is not on the allowlist.
const sysClone3 = 435

// sysSeccomp is seccomp(2). Go's syscall package froze its Linux syscall
// numbers before seccomp existed on amd64, so this one is written out; on
// arm64 the same constant comes from syscall. A wrong number here does not
// fail quietly — nothing installs, and every isolation test fails.
const sysSeccomp = 317

// archSyscalls are the allowed calls that exist only on this architecture,
// or that Go's frozen table omits here. The legacy names — open, stat, fork
// and the rest — have no equivalent on arm64, where the same operations are
// the *at and clone forms already in the portable list.
var archSyscalls = []int{
	syscall.SYS_ARCH_PRCTL, // sets the TLS base register; musl uses it at startup
	syscall.SYS_NEWFSTATAT,
	syscall.SYS_OPEN,
	syscall.SYS_STAT,
	syscall.SYS_LSTAT,
	syscall.SYS_ACCESS,
	syscall.SYS_DUP2,
	syscall.SYS_PIPE,
	syscall.SYS_POLL,
	syscall.SYS_SELECT,
	syscall.SYS_FORK,
	syscall.SYS_VFORK,
	syscall.SYS_MKDIR,
	syscall.SYS_RMDIR,
	syscall.SYS_UNLINK,
	syscall.SYS_RENAME,
	syscall.SYS_LINK,
	syscall.SYS_SYMLINK,
	syscall.SYS_READLINK,
	syscall.SYS_CHMOD,
	syscall.SYS_CHOWN,
	syscall.SYS_LCHOWN,
	syscall.SYS_EPOLL_CREATE,
	syscall.SYS_EPOLL_WAIT,
	syscall.SYS_EVENTFD,
	syscall.SYS_INOTIFY_INIT,
	syscall.SYS_SIGNALFD,
	syscall.SYS_TIME,
	syscall.SYS_ALARM,
	syscall.SYS_PAUSE,
	syscall.SYS_UTIME,
	syscall.SYS_UTIMES,
	syscall.SYS_FUTIMESAT,
	syscall.SYS_GETPGRP,

	// getrandom, another one Go's amd64 table predates. The number is from
	// the Go standard library's own internal/syscall/unix/sysnum_linux_amd64.go.
	// It is allowed on both architectures: a Go binary reads it at startup, so
	// denying it would mean no build could run anything Forge itself compiles.
	318,
}
