# Seccomp fuzzer — the filter proven for every input, not the ones a test named

The roadmap's five phases are done ([roadmap.md](roadmap.md)). This is the
first piece of work above them, and it makes the seccomp filter a security
artifact rather than a set of point tests: the filter is checked against its
own policy for the whole input space, and the interpreter every seccomp test
trusts is checked against the real kernel.

## What was unproven before

The filter is a hand-written classic-BPF program ([seccomp_linux.go](../internal/sandbox/seccomp_linux.go)).
Every test — and the checks in this repo rest on it heavily — runs it through
`evaluate`, a pure-Go BPF interpreter in [seccomp_linux_test.go](../internal/sandbox/seccomp_linux_test.go).
Two things that everything relied on were never actually shown:

1. **The BPF encoding implements the policy for every input.** The point tests
   name ~30 syscalls and a handful of clone flags. Nothing covered the rest of
   the number space, or the failure the code's own comment warns about — a jump
   offset that overflows the uint8 field once the allowlist passes 255 entries,
   an accidental fall-through, a mis-masked clone low word.
2. **`evaluate` matches the kernel.** The interpreter is a second
   implementation of the kernel's BPF machine, written by the same hand as the
   filter. If both share a blind spot — arg0 endianness, the arch prologue —
   every test agrees and every test is wrong.

## Deliverables

| # | Deliverable | Closes | Proven by |
|---|---|---|---|
| 1 | A fuzzer over `(arch, nr, arg0)` that compares the compiled filter against an independent statement of the policy | the number space outside the point tests; the jump-overflow / fall-through / mask class | `FuzzFilterMatchesPolicy` (seed corpus in the ordinary suite; `make fuzz` explores) |
| 2 | A differential test that runs the production filter for real and compares the kernel's errno to `evaluate`'s prediction | `evaluate` trusted but never grounded against the kernel | `TestInstalledFilterMatchesTheModel` (privileged, in the sandbox) |

## How each works

**The fuzzer** ([seccomp_fuzz_test.go](../internal/sandbox/seccomp_fuzz_test.go))
is Go's native `testing.F`: no generator, mutator, corpus or shrinker of our
own — the stdlib does all of it and records an escape to `testdata/fuzz` as a
permanent seed. The oracle, `policyResult`, reads the decision straight from
`allowedSyscalls` and the clone rule, with none of the filter's jump-and-mask
encoding, so agreement on a fuzzed input is evidence the encoding implements
the declared policy. The target touches no syscalls and needs no privileges,
so the seed corpus runs everywhere `make test` runs and the exploring form
(`make fuzz`, 30s in CI) needs only Go on Linux.

**The differential test** reuses the probe-in-the-sandbox pattern: a build
issues each kernel-facing syscall the filter denies, plus `getpid`, under the
real production filter, and prints the errno the kernel returned. The test
compares each to `evaluate`'s prediction for the same syscall. It is the one
test that closes the loop from the model to the kernel; without it the fuzzer
proves only that two things written here agree with each other.

## Not in this piece

- **A runtime fuzzer that fires random syscalls in a live process.** The
  differential is a curated, safe set on purpose: every syscall in it is
  refused by the filter before the kernel runs it, so a null argument is never
  read. Firing arbitrary numbers with arbitrary arguments for real is a way to
  corrupt or hang the process, not a way to test a filter, and the pure-Go
  fuzzer already covers the whole input space against the oracle.
- **The threat model and the CVE reproduction.** Named next in the plan; this
  piece is the fuzzer alone.

## Exit criteria

1. `make fuzz` explores and finds no escape (CI, `FUZZTIME=30s`).
2. The seed corpus passes in the ordinary suite on both architectures.
3. `TestInstalledFilterMatchesTheModel` passes: the kernel's errno matches the
   model for every audited syscall.

All three held on 2026-09-20: seeds and the differential test pass in the
colima VM (aarch64); `make fuzz` runs in CI.
