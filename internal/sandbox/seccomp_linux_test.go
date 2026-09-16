//go:build linux

package sandbox

import (
	"fmt"
	"syscall"
	"testing"
)

// evaluate runs the filter the way the kernel's BPF interpreter would, over
// the four instructions seccompFilter emits and a struct seccomp_data holding
// a syscall number, an architecture and the low word of the first argument.
//
// These tests need no privileges and no kernel support, which is the point:
// the policy — what is allowed, what is denied, what happens under another ABI
// — is checkable everywhere, and the privileged tests in
// sandbox_linux_test.go are left to prove the filter is actually installed.
func evaluate(f []syscall.SockFilter, arch uint32, nr int, arg0 uint32) (uint32, error) {
	var accumulator uint32
	for pc := 0; pc < len(f); {
		in := f[pc]
		switch in.Code {
		case bpfLoadWordAbs:
			switch in.K {
			case offsetNr:
				accumulator = uint32(nr)
			case offsetArch:
				accumulator = arch
			case offsetArg0:
				accumulator = arg0
			default:
				return 0, fmt.Errorf("instruction %d loads offset %d, which is not a field this filter reads", pc, in.K)
			}
			pc++
		case bpfJumpEqual:
			if accumulator == in.K {
				pc += 1 + int(in.Jt)
			} else {
				pc += 1 + int(in.Jf)
			}
		case bpfJumpSet:
			if accumulator&in.K != 0 {
				pc += 1 + int(in.Jt)
			} else {
				pc += 1 + int(in.Jf)
			}
		case bpfReturn:
			return in.K, nil
		default:
			return 0, fmt.Errorf("instruction %d has opcode %#x, which this filter never emits", pc, in.Code)
		}
	}
	// The kernel rejects a program that can fall off the end, so reaching
	// here means the filter would not have loaded at all.
	return 0, fmt.Errorf("execution ran past the last of %d instructions", len(f))
}

func TestFilterAllowsEverySyscallOnTheList(t *testing.T) {
	f := seccompFilter()
	for _, nr := range allowedSyscalls() {
		got, err := evaluate(f, auditArch, nr, 0)
		if err != nil {
			t.Fatalf("syscall %d: %v", nr, err)
		}
		if got != retAllow {
			t.Errorf("syscall %d returns %#x, want SECCOMP_RET_ALLOW (%#x)", nr, got, retAllow)
		}
	}
}

// TestFilterDeniesTheKernelFacingSurface names the syscalls the filter exists
// to stop. None of them is blocked by name anywhere in the implementation —
// they are denied by not being on the allowlist, and this test is what proves
// that mechanism works rather than that someone wrote a blocklist.
func TestFilterDeniesTheKernelFacingSurface(t *testing.T) {
	denied := map[string]int{
		"mount":           syscall.SYS_MOUNT,
		"umount2":         syscall.SYS_UMOUNT2,
		"pivot_root":      syscall.SYS_PIVOT_ROOT,
		"chroot":          syscall.SYS_CHROOT,
		"mknodat":         syscall.SYS_MKNODAT,
		"ptrace":          syscall.SYS_PTRACE,
		"unshare":         syscall.SYS_UNSHARE,
		"reboot":          syscall.SYS_REBOOT,
		"init_module":     syscall.SYS_INIT_MODULE,
		"delete_module":   syscall.SYS_DELETE_MODULE,
		"kexec_load":      syscall.SYS_KEXEC_LOAD,
		"swapon":          syscall.SYS_SWAPON,
		"syslog":          syscall.SYS_SYSLOG,
		"acct":            syscall.SYS_ACCT,
		"quotactl":        syscall.SYS_QUOTACTL,
		"settimeofday":    syscall.SYS_SETTIMEOFDAY,
		"adjtimex":        syscall.SYS_ADJTIMEX,
		"sethostname":     syscall.SYS_SETHOSTNAME,
		"add_key":         syscall.SYS_ADD_KEY,
		"keyctl":          syscall.SYS_KEYCTL,
		"perf_event_open": syscall.SYS_PERF_EVENT_OPEN,
		"clone3":          sysClone3,
	}
	f := seccompFilter()
	for name, nr := range denied {
		got, err := evaluate(f, auditArch, nr, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Against retAllow, not only against denyAction: comparing a syscall's
		// result with the constant that defines the policy passes whatever the
		// policy is set to, including "allow". A mutation that made denyAction
		// retAllow left this test green.
		if got == retAllow {
			t.Errorf("%s (%d) is allowed", name, nr)
		}
		if got != denyAction {
			t.Errorf("%s (%d) returns %#x, want the deny action %#x", name, nr, got, denyAction)
		}
	}
}

// TestDenialIsENOSYS pins the errno itself, because which one it is decides
// whether a C library retries with an older syscall or gives up. See the
// denyAction comment.
func TestDenialIsENOSYS(t *testing.T) {
	if action := denyAction >> 16; action != 0x0005 {
		t.Fatalf("deny action %#x is not SECCOMP_RET_ERRNO", denyAction)
	}
	if errno := syscall.Errno(denyAction & 0xffff); errno != syscall.ENOSYS {
		t.Fatalf("denied syscalls return %v, want ENOSYS", errno)
	}
}

// TestFilterDeniesCloneWithNamespaceFlags is the policy half of bounding
// clone. A fork and a thread pass; any one CLONE_NEW* bit is refused with
// EPERM rather than ENOSYS, because the syscall is not missing — this use of
// it is. The refusal is checked one flag at a time so that a mask missing a
// single namespace is a single failing case rather than a green run.
func TestFilterDeniesCloneWithNamespaceFlags(t *testing.T) {
	f := seccompFilter()
	const sigchld = uint32(syscall.SIGCHLD)
	allowed := map[string]uint32{
		"fork":   sigchld,
		"thread": syscall.CLONE_VM | syscall.CLONE_FS | syscall.CLONE_FILES | syscall.CLONE_SIGHAND | syscall.CLONE_THREAD | syscall.CLONE_SYSVSEM | syscall.CLONE_SETTLS,
		"vfork":  syscall.CLONE_VM | syscall.CLONE_VFORK | sigchld,
	}
	for name, flags := range allowed {
		got, err := evaluate(f, auditArch, syscall.SYS_CLONE, flags)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != retAllow {
			t.Errorf("clone as a %s (flags %#x) returns %#x, want SECCOMP_RET_ALLOW", name, flags, got)
		}
	}

	namespaces := map[string]uint32{
		"CLONE_NEWNS":     syscall.CLONE_NEWNS,
		"CLONE_NEWUTS":    syscall.CLONE_NEWUTS,
		"CLONE_NEWIPC":    syscall.CLONE_NEWIPC,
		"CLONE_NEWUSER":   syscall.CLONE_NEWUSER,
		"CLONE_NEWPID":    syscall.CLONE_NEWPID,
		"CLONE_NEWNET":    syscall.CLONE_NEWNET,
		"CLONE_NEWCGROUP": syscall.CLONE_NEWCGROUP,
	}
	for name, flag := range namespaces {
		got, err := evaluate(f, auditArch, syscall.SYS_CLONE, flag|sigchld)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == retAllow {
			t.Errorf("clone with %s is allowed", name)
		}
		if want := retErrno | uint32(syscall.EPERM); got != want {
			t.Errorf("clone with %s returns %#x, want EPERM (%#x)", name, got, want)
		}
	}
}

// TestFilterKillsAnotherABI uses a syscall that is allowed under this
// architecture: it is the architecture that has to be what decides, not the
// number, or a 32-bit process could reach a denied syscall through a number
// that happens to be allowed under the 64-bit table.
func TestFilterKillsAnotherABI(t *testing.T) {
	const auditArchI386 = 0x40000003
	f := seccompFilter()
	got, err := evaluate(f, auditArchI386, syscall.SYS_READ, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != retKillProcess {
		t.Fatalf("read under the i386 ABI returns %#x, want SECCOMP_RET_KILL_PROCESS (%#x)", got, uint32(retKillProcess))
	}
}

func TestFilterFitsTheKernelLimit(t *testing.T) {
	f := seccompFilter()
	if want := 2*len(allowedSyscalls()) + 8; len(f) != want {
		t.Errorf("filter is %d instructions, want %d: four of prologue, two per syscall, three more for clone, one deny", len(f), want)
	}
	if len(f) > maxFilterLen {
		t.Errorf("filter is %d instructions, over the kernel's limit of %d", len(f), maxFilterLen)
	}
}

// TestAllowlistHasNoDuplicates catches the same syscall being added twice —
// harmless to the kernel, but it means one of the two entries was added
// without reading the list, and the next such edit may not be harmless.
func TestAllowlistHasNoDuplicates(t *testing.T) {
	seen := make(map[int]bool)
	for _, nr := range allowedSyscalls() {
		if seen[nr] {
			t.Errorf("syscall %d is on the allowlist twice", nr)
		}
		seen[nr] = true
	}
}
