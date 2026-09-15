# Phase 0 — definition of done

Phase 0 is the part of Forge that runs one untrusted command on one machine
and contains it: the sandbox runtime and the pool that will drive it. Nothing
distributed — no API, no queue, no leases — belongs to it. Written on
2026-09-15, after the five named weeks were merged, so this is the bar the
work is held to rather than the bar it was planned against; the gap between
the two is recorded in [decisions.md](decisions.md).

## Done means

Every row has a check that fails if the claim stops being true. A row without
one is not done.

| Deliverable | Proven by |
|---|---|
| Bounded worker pool; drain and cancel are different shutdowns | `make test` — `internal/pool`, 11 tests, race detector on |
| Fresh mount, PID, UTS, IPC, net and user namespaces per job; `pivot_root` with the old root detached | `TestEveryNamespaceIsNew`, `TestHostFilesystemIsUnreachable`, `TestMountsDoNotLeakToHost`, `TestCommandRunsAsPIDOne` |
| A `/dev` of six bind-mounted nodes and four symlinks, nothing else | `TestDevIsOnlyTheMinimalSet`, `TestDevNodesBehaveLikeTheirKernelDevices` |
| cgroups v2 memory, cpu and pids limits, applied at clone time; an OOM kill is reported as one | `internal/cgroup`, `TestMemoryLimitIsReportedAsAnOOMKill`, `TestSandboxRunsInsideItsOwnCgroup` |
| Deny-by-default seccomp allowlist, `ENOSYS` on denial, architecture checked before the number, `no_new_privs` | `TestSeccompDenies*`, `TestSeccompFilterSurvivesExec`, `seccomp_linux_test.go` |
| Sandbox root is host uid 524288; the image is not writable from inside; `setgroups` denied | `TestSandboxRootIsAnUnprivilegedHostUid`, `TestTheRootfsImageIsNotWritable`, `TestSetgroupsIsDeniedInTheNamespace` |
| Exit 125 means "the sandbox failed to build", distinct from the command's own status | `TestExitStatusPropagates`, `spec_test.go` |
| No third-party dependencies | `go.mod` has no `require` block |

## Exit criteria

All four held on 2026-09-15 at the commit this file was added in.

1. **CI green on x86_64 with the privileged tests running, not skipped.**
   `make test-sandbox` on `ubuntu-latest`. The log has to be read — an
   all-skipped run also reports PASS.
2. **`make vm-test` green on aarch64.** The same binaries, inside the colima VM.
   Sandbox and cgroup suites: 63 passes, 0 skips, 0 failures.
3. **`make mutate` catches every mutation it has.** 22 mutations across
   `/dev`, the filter and the user namespace; 22 caught. One
   (`no-user-namespace`) is caught by the 180 s deadline rather than by an
   assertion, because Go's fork path deadlocks before any test runs; the
   harness prints it as such. Two mutations from earlier weeks were retired
   when this harness moved into the repo: `MS_NODEV` on the `/dev` tmpfs had
   become a no-op once the nodes were bind mounts (so the flag is now set and
   the mutation is meaningless), and dropping `TSYNC` cannot be pinned by any
   test because which thread reaches `execve` is the scheduler's choice. The
   cgroup mutations (8 of 8 caught on 2026-09-09) were never saved as a
   script and are not part of this number.
4. **The docs describe what is built.** `architecture.md` marks every section
   *implemented* or not; `README.md` quotes no number a harness did not
   produce.

## Not in Phase 0

Named here so the next phase starts from a list rather than from memory. Each
is also in the README's *Limitations*.

- **Per-job rootfs.** One image and one uid map are shared by every job on a
  worker, so the boundary is build-to-host, not build-to-build; `/tmp` in the
  image is writable and outlives a job. Wants an overlay.
- **`clone` flags are unbounded.** The filter reads syscall numbers only, so a
  build can create namespaces of its own.
- **The capability bounding set is not dropped.** A build holds a full set
  inside its namespace, bounded by the map and the filter, not by `prctl`.
- **No `/dev/ptmx`, no `/dev/shm`.** A build that wants a pty or POSIX shared
  memory does not get one.
- **`Run` discards `group.Close()`'s error** (`sandbox_linux.go`), unchanged
  since Week 3.
