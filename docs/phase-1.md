# Phase 1 — finish the sandbox

Phase 0 built the sandbox's primitives and wrote down what it left open
([phase-0.md](phase-0.md), *Not in Phase 0*). Phase 1 closes the items on
that list that concern the sandbox itself, in the order that each one makes
the next safer, before anything above the sandbox is built.

## Deliverables

| # | Deliverable | Closes | Proven by |
|---|---|---|---|
| 1 | Per-job overlay root filesystem: the image is never written, no state outlives a job, no host-side per-job directory | one image shared by every job; `/tmp` outliving a job | `TestTheImageIsNeverWritten`, `TestNoWritableStateOutlivesAJob`, `TestAPackageManagerCanChangeTheInstalledSet`, `TestTheLayerCountsAgainstTheMemoryLimit` |
| 2 | `clone` argument filtering: a build cannot create namespaces of its own; `clone3` stays denied by number | `clone` flags unbounded | `TestFilterDeniesCloneWithNamespaceFlags` (policy), `TestABuildCannotCreateANamespace` (installed) |
| 3 | Capability bounding set dropped to what a build needs: `CAP_SYS_ADMIN` and the rest of the kernel-facing set are not held after `execve` | full capability set inside the namespace | `TestTheBoundingSetIsTheKeepList`, `TestADroppedCapabilityIsGone` |

Each deliverable carries mutations in `hack/mutate.py`; `make mutate` is
the evidence the tests assert what the table says. 27 scored, 27 caught on
2026-09-16; two more are listed unscored, with the reason beside each.

## Order and why

1. **Overlay first.** It changes what "the image" means for every later
   test — a test that writes has to be run on a layer, not on the image —
   so the test helpers it forces (`scratchImage`) are in place before the
   next two add tests of their own.
2. **`clone` flags before the bounding set.** Creating a nested user
   namespace is the first step of the standard user-namespace exploit
   chain; it is bounded by the filter and not by any capability, so the
   filter is the one that has to move.
3. **Bounding set last.** It removes `CAP_SYS_ADMIN` from the build, which
   the two items above never depended on, and its keep-list is decided by
   what the tests from 1 and 2 already prove a build does: chown, chmod,
   unlink files it does not own.

## Not in Phase 1

- **`/dev/ptmx`, `/dev/shm`.** No build has needed them; the day one does
  is the day they are added, with the test that needed them.
- **Artifacts.** Nothing a build writes survives the job. The upper layer
  on a host directory that Phase 1 rejected is the natural place for a
  later phase to collect from.
- **Per-job uid ranges.** The overlay means no two jobs share a writable
  filesystem, so a shared map separates nothing today.
- **`Run` discards `group.Close()`'s error** (`sandbox_linux.go`),
  unchanged since Phase 0 Week 3.
- **Everything above the sandbox** — the worker's job loop, the queue,
  the API — is Phase 2 onward.

## Exit criteria

All four held on 2026-09-16 at the commit this file was last changed in.

1. **CI green on x86_64 with the privileged tests running, not skipped.**
   71 passes, 0 skips, 0 failures, read from the log.
2. **`make vm-test` on aarch64: 0 skips, 0 failures.** 71 passes.
3. **`make mutate` exits 0.** 27/27.
4. **Every item in the Phase 0 *Not in Phase 0* list is either closed by a
   row above or named in *Not in Phase 1* with a reason.** Five items:
   three closed, two carried.
