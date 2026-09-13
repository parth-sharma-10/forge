//go:build linux && arm64

package sandbox

import "syscall"

// auditArch is AUDIT_ARCH_AARCH64: EM_AARCH64, plus the 64-bit and
// little-endian bits. See the amd64 file for what the comparison is for.
const auditArch = 0xc00000b7

// sysSeccomp is seccomp(2), which Go's arm64 table does define.
const sysSeccomp = syscall.SYS_SECCOMP

// archSyscalls are the allowed calls this architecture spells differently.
// There are no legacy forms here: arm64 shipped with the *at syscalls, so
// open, stat and fork simply do not exist and the portable list already
// carries the calls that replaced them.
var archSyscalls = []int{
	// The same syscall amd64 calls newfstatat.
	syscall.SYS_FSTATAT,
	syscall.SYS_GETRANDOM,
}
