//go:build linux

package sandbox

import (
	"syscall"
	"testing"
)

// FuzzFilterMatchesPolicy fuzzes the seccomp filter against an independent
// statement of the same policy. The filter is a hand-written BPF program that
// encodes the allowlist as jumps and masks; the oracle below reads the policy
// straight from allowedSyscalls and the clone rule, with none of that
// encoding. Agreement on a fuzzed (arch, nr, arg0) is evidence the encoding
// implements the policy for every input, not only the ones a point test
// happened to name — the class of bug the code warns about (a jump offset that
// overflows the uint8 field past 255 entries, an accidental fall-through, the
// low-word masking of clone's flags) shows up here as a divergence.
//
// The fuzz target touches no syscalls and needs no privileges: it runs the
// filter through the same evaluate interpreter the point tests use. Without
// -fuzz it still runs, over the seed corpus below, in the ordinary suite; with
// -fuzz it explores outward and writes any escape to testdata/fuzz as a
// permanent regression seed. `make fuzz` runs the exploring form.
func FuzzFilterMatchesPolicy(f *testing.F) {
	const auditArchI386 = 0x40000003

	// One seed per boundary the policy draws, so the corpus alone covers them
	// and the fuzzer starts from real edges rather than zero.
	for _, nr := range allowedSyscalls() {
		f.Add(uint32(auditArch), nr, uint32(0))
	}
	for _, nr := range deniedSurface() {
		f.Add(uint32(auditArch), nr, uint32(0))
	}
	// clone: allowed as a fork/thread, denied for any one namespace bit.
	f.Add(uint32(auditArch), syscall.SYS_CLONE, uint32(syscall.SIGCHLD))
	for _, flag := range []int{
		syscall.CLONE_NEWNS, syscall.CLONE_NEWUTS, syscall.CLONE_NEWIPC,
		syscall.CLONE_NEWUSER, syscall.CLONE_NEWPID, syscall.CLONE_NEWNET,
		syscall.CLONE_NEWCGROUP,
	} {
		f.Add(uint32(auditArch), syscall.SYS_CLONE, uint32(flag)|uint32(syscall.SIGCHLD))
	}
	// A wrong ABI is killed before the number is read.
	f.Add(uint32(auditArchI386), syscall.SYS_READ, uint32(0))
	f.Add(uint32(0), syscall.SYS_READ, uint32(0))
	// Numeric edges of the syscall field.
	for _, nr := range []int{0, -1, 511, 1<<31 - 1} {
		f.Add(uint32(auditArch), nr, uint32(0))
	}

	filter := seccompFilter()
	allowed := allowedSet()

	f.Fuzz(func(t *testing.T, arch uint32, nr int, arg0 uint32) {
		got, err := evaluate(filter, arch, nr, arg0)
		if err != nil {
			// The kernel rejects a filter that can fall off the end or holds
			// an opcode it does not understand; either means the filter would
			// never have loaded, so it is a real defect, not a bad input.
			t.Fatalf("filter is malformed at (arch=%#x nr=%d arg0=%#x): %v", arch, nr, arg0, err)
		}
		want := policyResult(arch, nr, arg0, allowed)
		if got != want {
			t.Fatalf("filter(arch=%#x nr=%d arg0=%#x) = %#x, policy says %#x", arch, nr, arg0, got, want)
		}
	})
}

// policyResult is the allowlist stated as a decision, independent of how
// seccompFilter encodes it. The order matches the filter's: the ABI is
// checked before the number, clone is decided on its flags, everything else on
// the number alone.
func policyResult(arch uint32, nr int, arg0 uint32, allowed map[int]bool) uint32 {
	if arch != auditArch {
		return retKillProcess
	}
	if nr == syscall.SYS_CLONE {
		if arg0&cloneNamespaceFlags != 0 {
			return cloneDenyAction
		}
		return retAllow
	}
	if allowed[nr] {
		return retAllow
	}
	return denyAction
}

// allowedSet is the allowlist as a set, for the oracle's number check. clone
// is in it but never reached through it: policyResult decides clone first.
func allowedSet() map[int]bool {
	m := make(map[int]bool)
	for _, nr := range allowedSyscalls() {
		m[nr] = true
	}
	return m
}

// deniedSurface is the kernel-facing syscalls the filter exists to stop, named
// so the seed corpus starts from them. It is the same list TestFilter-
// DeniesTheKernelFacingSurface pins, kept here as seeds rather than shared to
// keep the two independent.
func deniedSurface() []int {
	return []int{
		syscall.SYS_MOUNT, syscall.SYS_UMOUNT2, syscall.SYS_PIVOT_ROOT,
		syscall.SYS_CHROOT, syscall.SYS_MKNODAT, syscall.SYS_PTRACE,
		syscall.SYS_UNSHARE, syscall.SYS_REBOOT, syscall.SYS_INIT_MODULE,
		syscall.SYS_DELETE_MODULE, syscall.SYS_KEXEC_LOAD, syscall.SYS_SWAPON,
		syscall.SYS_SYSLOG, syscall.SYS_ACCT, syscall.SYS_QUOTACTL,
		syscall.SYS_SETTIMEOFDAY, syscall.SYS_ADJTIMEX, syscall.SYS_SETHOSTNAME,
		syscall.SYS_ADD_KEY, syscall.SYS_KEYCTL, syscall.SYS_PERF_EVENT_OPEN,
		sysClone3,
	}
}
