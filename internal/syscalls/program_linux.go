//go:build linux

package syscalls

import (
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
)

// enosysTag marks a counts key as an ENOSYS return seen at sys_exit rather
// than a call seen at sys_enter. Both programs share one counts map; the tag
// keeps their keys apart, and drain splits them back into two metrics.
const enosysTag = uint64(1) << 63

// enosys is -ENOSYS as the tracepoint reports a syscall's return value.
const enosys = -38

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
	insns := enrolledOnly(enrolled)
	// R8 = syscall number = ctx->args[1].
	insns = append(insns, asm.LoadMem(asm.R8, asm.R6, 8, asm.DWord))
	return append(insns, increment(counts)...)
}

// buildExitProgram counts the syscalls a job made that returned ENOSYS, on the
// raw_syscalls:sys_exit tracepoint. It exists because sys_enter cannot see a
// syscall the seccomp filter denies: the filter runs first and the call skips
// the enter tracepoint. The exit tracepoint still fires for it, carrying the
// filter's ENOSYS. ENOSYS is also what the kernel returns for a syscall it
// does not implement, so the count is "denied or unimplemented", which for a
// build is one thing: it asked for something it could not have.
//
// A tracepoint program, not a raw one, because the tracepoint has already
// decoded the syscall number (syscall_get_nr) into its record: {common header,
// 8 bytes; long id at 8; long ret at 16}. The raw form would have to read the
// number out of pt_regs at an offset that differs per architecture.
func buildExitProgram(enrolled, counts *ebpf.Map) asm.Instructions {
	insns := enrolledOnly(enrolled)
	insns = append(insns,
		// Only ENOSYS: R8 = ret, and anything else is not ours to count.
		asm.LoadMem(asm.R8, asm.R6, 16, asm.DWord),
		asm.JNE.Imm(asm.R8, enosys, "out"),
		// R8 = id | enosysTag. The tag is bit 63, which no immediate can
		// carry, so it is built in R9.
		asm.LoadMem(asm.R8, asm.R6, 8, asm.DWord),
		asm.Mov.Imm(asm.R9, 1),
		asm.LSh.Imm(asm.R9, 63),
		asm.Or.Reg(asm.R8, asm.R9),
	)
	return append(insns, increment(counts)...)
}

// enrolledOnly is both programs' prologue: R6 = ctx, R7 = the current task's
// cgroup id, and a jump to "out" unless that cgroup is an enrolled job.
func enrolledOnly(enrolled *ebpf.Map) asm.Instructions {
	const enrolledKey = -8 // u64 cgroup id
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
	}
}

// increment is both programs' tail: counts[(R7, R8)] += 1, and the "out"
// label the prologue jumps to.
func increment(counts *ebpf.Map) asm.Instructions {
	const (
		countsKey = -16 // {u64 cgroup id, u64 syscall nr}, so cgid at -16, nr at -8
		oneValue  = -24 // u64 = 1, for the first touch of a counts key
	)
	return asm.Instructions{
		// counts[(cgid, nr)]? Build the 16-byte key on the stack.
		asm.StoreMem(asm.RFP, countsKey, asm.R7, asm.DWord),
		asm.StoreMem(asm.RFP, countsKey+8, asm.R8, asm.DWord),
		asm.Mov.Reg(asm.R2, asm.RFP),
		asm.Add.Imm(asm.R2, countsKey),
		asm.LoadMapPtr(asm.R1, counts.FD()),
		asm.FnMapLookupElem.Call(),
		asm.JNE.Imm(asm.R0, 0, "inc"), // already counted once: increment.

		// First touch of this (cgid, nr): set the count to 1. Written through a
		// register — a BPF store-immediate has no double-word form — using R0,
		// which holds the null lookup result here and is clobbered by the
		// update call's return regardless.
		asm.Mov.Imm(asm.R0, 1),
		asm.StoreMem(asm.RFP, oneValue, asm.R0, asm.DWord),
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

		// out: both tracepoint kinds ignore the return value; 0 by convention.
		asm.Mov.Imm(asm.R0, 0).WithSymbol("out"),
		asm.Return(),
	}
}
