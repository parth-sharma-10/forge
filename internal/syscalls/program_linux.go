//go:build linux

package syscalls

import (
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
)

// buildProgram assembles the raw_tracepoint/sys_enter program by hand, in the
// asm package, rather than compiling C with clang and embedding an object. The
// program is small enough — one helper call, two map lookups, an increment —
// that hand-assembly avoids a build-time C toolchain and a committed binary
// blob while still using cilium/ebpf as the loader and verifier front-end. It
// is the same reasoning as the hand-written seccomp filter, one size up.
//
// The context of a raw_tracepoint on sys_enter is bpf_raw_tracepoint_args,
// whose args are (struct pt_regs *regs, long id): the syscall number is the
// second, at byte offset 8, and is read directly from the context.
//
// The logic: look up the current task's cgroup id in the enrolled map; if it
// is absent the syscall is not a forge job and is ignored. Otherwise increment
// counts[(cgroup_id, syscall_nr)] — a per-CPU hash, so the increment needs no
// atomic and no lock: the helper returns this CPU's own slot.
func buildProgram(enrolled, counts *ebpf.Map) asm.Instructions {
	const (
		enrolledKey = -8  // u64 cgroup id
		countsKey   = -16 // {u64 cgroup id, u64 syscall nr}, so cgid at -16, nr at -8
		oneValue    = -24 // u64 = 1, for the first touch of a counts key
	)
	return asm.Instructions{
		// R6 = ctx (callee-saved across the helper calls below).
		asm.Mov.Reg(asm.R6, asm.R1),
		// R7 = bpf_get_current_cgroup_id().
		asm.FnGetCurrentCgroupId.Call(),
		asm.Mov.Reg(asm.R7, asm.R0),

		// enrolled[cgid]? Key is the cgroup id on the stack.
		asm.StoreMem(asm.RFP, enrolledKey, asm.R7, asm.DWord),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, enrolledKey),
		asm.LoadMapPtr(asm.R1, enrolled.FD()),
		asm.FnMapLookupElem.Call(),
		asm.JEq.Imm(asm.R0, 0, "out"), // not a forge job: ignore.

		// R8 = syscall number = ctx->args[1].
		asm.LoadMem(asm.R8, asm.R6, 8, asm.DWord),

		// counts[(cgid, nr)]? Build the 16-byte key on the stack.
		asm.StoreMem(asm.RFP, countsKey, asm.R7, asm.DWord),
		asm.StoreMem(asm.RFP, enrolledKey, asm.R8, asm.DWord),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, countsKey),
		asm.LoadMapPtr(asm.R1, counts.FD()),
		asm.FnMapLookupElem.Call(),
		asm.JNE.Imm(asm.R0, 0, "inc"), // already counted once: increment.

		// First touch of this (cgid, nr): set the count to 1.
		asm.StoreImm(asm.RFP, oneValue, 1, asm.DWord),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, countsKey),
		asm.Mov.Reg(asm.R3, asm.RFP),
		asm.Add.Imm(asm.R3, oneValue),
		asm.Mov.Imm(asm.R4, 0), // BPF_ANY
		asm.LoadMapPtr(asm.R1, counts.FD()),
		asm.FnMapUpdateElem.Call(),
		asm.Ja.Label("out"),

		// inc: *(u64 *)value += 1. Per-CPU, so this CPU's slot alone.
		asm.LoadMem(asm.R1, asm.R0, 0, asm.DWord).WithSymbol("inc"),
		asm.Add.Imm(asm.R1, 1),
		asm.StoreMem(asm.R0, 0, asm.R1, asm.DWord),

		// out: raw tracepoints ignore the return value; 0 by convention.
		asm.Mov.Imm(asm.R0, 0).WithSymbol("out"),
		asm.Return(),
	}
}
