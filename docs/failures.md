# Failures

Debugging log. Every failure that cost real time gets an entry, in this format:

```
## <date> — <title>
Problem / Symptoms / Root cause / Investigation / Fix / Lesson
```

Entries are written when the failure happens, not reconstructed later.

---

## 2026-09-08 — Send on closed channel in the pool's shutdown path

**Problem.** `Pool.Close()` closes the task channel so workers can drain it with
a plain `range`. A concurrent `Submit` parked on a send to that channel makes
the close a panic: `send on closed channel`.

**Symptoms.** None in production — this was caught at design time and then
*reproduced deliberately* to prove the test suite catches it. Removing the
`p.senders.Wait()` line from `Close` and running
`go test -race -run TestConcurrentSubmitAndClose` panics inside `Submit` at
`pool.go:129`, the blocking send.

**Root cause.** Closing a channel is only safe when no sender can still be
holding a reference and about to send. A `closed bool` checked before the send
is not enough: the check and the send are not atomic, so a sender can pass the
check, get descheduled, and wake up after the channel is gone.

**Investigation.** Two candidate fixes. (1) Never close the channel; signal
shutdown with a separate `closed` channel and have workers drain with a
non-blocking receive. That loses the guarantee that an accepted task runs — a
task submitted in the window between `Close` and the worker's final `default:`
is silently dropped. (2) Make the close wait for senders.

**Fix.** `Submit` registers in a `sync.WaitGroup` under `mu.RLock()` after
checking the closed flag; `Close` sets the flag under `mu.Lock()`, releases,
`senders.Wait()`s, and only then closes the channel. The lock orders the flag
check against the registration, and the WaitGroup guarantees no registered
sender remains. A sender parked on a full queue still exits, because its
`select` watches both the caller's context and the pool's root context.

**Lesson.** "Who is allowed to close this channel, and how do they know no one
is still sending?" is the question to answer *before* writing the close, not
after the panic. The general rule holds: the sender closes, and if there are
many senders you need a barrier that turns them into zero senders first.

**Verification.** The mutation is part of the record — `TestConcurrentSubmitAndClose`
runs 50 rounds of 16 concurrent submitters against a concurrent `Close`, and it
fails without the barrier. A test that has never been seen to fail is not
evidence.

---

## 2026-09-08 — Graceful drain delayed the hard stop by a full task duration

**Problem.** `cmd/forge-worker` handles the first SIGINT as a drain and the
second as a cancel. With a small queue, the second signal took about a second to
have any effect.

**Symptoms.** Timestamps from a real run (`-workers=2 -queue=2 -tasks=100
-task-duration=1s`), second SIGINT sent at ~16.3s:

```
20:53:16.024 INFO  signal received, draining: no new work accepted
20:53:17.027 WARN  second signal, cancelling work in flight     <- ~1s late
```

**Root cause.** The signal goroutine is `<-sigs; p.Close(); <-sigs; cancel()`.
`Close` waits for in-flight `Submit` calls to leave before closing the task
channel, and the main goroutine was parked on a send to a full queue. That
`Submit` only unparked when a worker finished a 1s task, so `Close` — and the
second `<-sigs` behind it — stalled for the length of a task. Under Kubernetes
the same shape means the drain eats the termination grace period and the pod
gets SIGKILLed mid-build instead of escalating cleanly.

**Investigation.** Found by running the binary rather than by a test: the test
suite only ever called `Close` from the main goroutine, where the stall is
invisible. Comparing the log timestamp against the `kill -INT` time is what made
it obvious. Two candidate fixes: run `Close` in its own goroutine in the signal
handler (hides the stall, keeps `Close` slow for every other caller), or make
`Close` release parked senders (fixes it for all callers).

**Fix.** Added a `closing` channel that `Close` closes *before* waiting for
senders. `Submit`'s select now watches it, so a parked `Submit` returns
`ErrClosed` immediately instead of waiting for a queue slot. The
sender-barrier ordering is unchanged, so the send-on-closed-channel guarantee
from the previous entry still holds.

**Lesson.** Shutdown latency is a property you have to *measure*, not read off
the code. The unit tests all passed both before and after; only wall-clock
timestamps from a real signal exposed it. `TestCloseReleasesParkedSubmit` now
asserts the timing, and fails against the old implementation.

---

## 2026-09-09 — A rootfs symlink that is dangling right up until pivot_root

**Problem.** The sandbox test guard checks that `$FORGE_TEST_ROOTFS` really is a
root filesystem before running, by stat'ing `bin/sh` inside it. Every sandbox
test failed at that guard against a freshly unpacked, perfectly good Alpine
rootfs: `stat /var/tmp/forge-rootfs/bin/sh: no such file or directory`.

**Symptoms.** `ls -l` showed the file present — `bin/sh` as a symlink to
`/bin/busybox`, and `bin/busybox` as a 919KB executable right next to it — while
`test -e /var/tmp/forge-rootfs/bin/sh` from the host shell also said no.

**Root cause.** `/bin/sh` is an **absolute** symlink to `/bin/busybox`. Absolute
symlinks inside a root filesystem resolve against the *resolver's* root, not
against the directory they live in. From the host that means `/bin/busybox` on
Ubuntu, which does not exist — so the link is dangling, and `os.Stat`, which
follows symlinks, is correct to say so. It stops being dangling the instant
`pivot_root` makes that rootfs `/`.

**Fix.** `os.Lstat` in the guard: ask whether the entry exists, not whether its
target resolves. One character of API, and the reason is now a comment above it.

**Investigation note.** The first suspicion was a failed `tar` extraction, and
re-extracting "fixed" nothing while looking like it might have. Listing the link
and its target separately is what separated the two hypotheses — a missing file
and an unresolvable one look identical through `os.Stat`.

**Lesson.** A rootfs is only self-consistent from inside itself. Any host-side
check against one has to be `Lstat`-shaped, and the same trap waits for anything
that inspects paths in an image before entering it. Related: the sandbox's own
`exec.LookPath` is correct only because it runs *after* `pivot_root`.

---

## 2026-09-09 — A mutation test leaked a mount and made the next mutation pass

**Problem.** Six mutations of `internal/sandbox` were run to prove the tests
catch them. Five failed as expected. Deleting the rootfs bind mount — the line
that exists solely to satisfy `pivot_root`'s "new root must be a mount point"
requirement — left the whole suite **passing**.

**Symptoms.** `findmnt` on the VM, after the mutation batch:

```
/var/tmp/forge-rootfs   /dev/vda1[/var/tmp/forge-rootfs]   shared
```

A bind mount of the rootfs, in the *host* mount namespace, owned by nothing.

**Root cause.** Two bugs stacked, and the first created the second.

1. An earlier mutation in the same batch removed the
   `mount("none", "/", MS_REC|MS_PRIVATE)` call. The VM's `/` is `shared:1`, so
   the child's bind mount propagated straight back into the host's peer group
   and outlived the namespace that made it. That mutation was correctly reported
   as caught — but its side effect persisted.
2. That leftover mount then made `/var/tmp/forge-rootfs` a mount point for free,
   so the *next* mutation, which removes the code that creates exactly that
   property, had nothing left to break.

**Investigation.** The gap was on a security property, so "the test is weak" and
"the environment is lying" had to be separated rather than guessed between.
Unmounting the leak and re-running the same mutation on a clean host gave
`pivot_root /var/tmp/forge-rootfs: device or resource busy` — EBUSY, not the
EINVAL the man page's wording suggests, which is itself worth knowing.
Re-creating the leak deliberately and running the *unmutated* binary confirmed
the other half: `TestMountsDoNotLeakToHost` does fail on a dirty host. No test
was weak; only the order the mutations ran in hid one.

**Fix.** The test guard now refuses to run at all when the rootfs is already
mounted in the host namespace, naming the `umount` to run. A leak can no longer
be silently inherited by the next run — it fails loudly at the first test.

**Lesson.** A mutation that changes global state has to be cleaned up before the
next one, or the following experiment measures the residue instead of the code.
Test isolation is not only about parallel tests: sequential runs share a kernel.
And the reason the leak was possible at all is the thing the code under test
exists to prevent — `MS_PRIVATE` is not defensive tidiness, it is the difference
between a sandbox and a mount the host is now stuck with.

---

## 2026-09-09 — Counting processes in a forking shell is a race

**Problem.** `TestProcIsTheSandboxProcessTable` asserted that a sandboxed
`ls /proc | grep -c '^[0-9]'` reports one or two processes rather than the
host's hundreds. It passed once, then failed on every subsequent run with 3.

**Root cause.** The assertion counted something the test itself creates. The
script is `cat /proc/1/comm; ls /proc | grep -c ...`, and busybox `sh` forks for
each stage of the pipeline, so `ls` reads `/proc` with `sh`, `ls` and `grep` all
live — three entries, or fewer if a stage has already exited. The exact count is
a scheduling outcome.

**Fix.** Assert something the sandbox cannot influence: the host test process's
own PID has no `/proc` entry inside. Deterministic, and it fails far more
legibly — dropping `CLONE_NEWPID` now yields `"systemd\nLEAKED"` against a want
of `"sh\nISOLATED"`, which names the bug in the diff. Ten consecutive suite runs
are clean.

**Lesson.** An assertion about "how many" is usually an assertion about timing
wearing a disguise. Prefer a fact the code under test either exposes or hides —
presence or absence — over a count of things the test itself spawned.

---

## 2026-09-09 — The cgroup tests passed while leaking a cgroup per test

**Problem.** The first full run of the cgroups v2 suite was green. Three tests
took 1.04s each while doing nothing that should take a second.

**Symptoms.** 1.04s is not a number a test arrives at by accident — it is
exactly the ceiling of `Group.remove`'s retry loop, 100 attempts at 10ms. So
every one of those tests was failing to remove its cgroup, retrying for a full
second, giving up, and returning an error that `defer g.Close()` discarded.
`findmnt`'s equivalent for cgroups confirmed it:

```
/sys/fs/cgroup/forge-test-67099/job-2905759006
/sys/fs/cgroup/forge-test-67099/job-252913528
/sys/fs/cgroup/forge-test-67099/job-2110793459
/sys/fs/cgroup/forge-test-67099/job-701056961
```

Four cgroups the suite created and never removed, on a green run.

**Root cause.** Two, one in the tests and one in the code they were testing.

1. `defer g.Close()` throws away the only signal that cleanup failed. Every
   test did it, so no test could see a leak.
2. `prepareRoot` created its root directory and then, if the controller check
   failed, returned the error and left the directory behind. That orphan then
   made *its own parent* un-removable, which is what the retry loop was
   grinding against in `TestCreateRejectsAnUndelegatedRoot`.

The test-side ordering bug was mine too: `defer g.Close()` runs before
`t.Cleanup`, so the processes a test started were still alive — and still
members of the cgroup — when it tried to remove it.

**Fix.** `closeGroup(t, g)` fails the test if removal fails; `prepareRoot`
removes a root it created but could not configure, and never touches one it was
handed; the started processes are stopped by a `defer` registered *after* the
close, so LIFO order kills them first. The 1.04s tests are now 0.02s to 0.26s —
the timing was the tell, and it went away when the cause did.

**Lesson.** A cleanup function whose error is discarded is not cleanup, it is a
comment. The leak was not subtle and not rare — it happened on every run — and
it was invisible purely because nothing asserted on it. Where the failure mode
is "the resource is still there afterwards", the test has to look afterwards.

---

## 2026-09-09 — Mutation testing lied twice: stale binaries, then recycled pids

**Problem.** The mutation batch over the cgroup code reported different results
on identical consecutive runs — 3 gaps, then 2 different gaps, then 2 others.
A mutation-testing harness that is not reproducible is worth nothing.

**Symptoms.** Run to run, `controllers never enabled on the root`,
`prepareRoot leaks a root it failed to configure` and `no EBUSY retry` each
flipped between caught and missed. Meanwhile every one of them, run on its own,
was caught.

**Root cause.** Two independent defects, both of them "the harness measured
something other than the mutation".

1. **Stale binaries.** Four of the mutations did not compile — deleting a line
   left a variable or an import unused. The harness ignored the build's exit
   status, so the *previous* mutation's binary ran instead, and whatever it
   reported was attributed to the current mutation. This is the same trap as
   the Week 2 mount leak, rebuilt from scratch one week later.
2. **Recycled pids.** The test roots were named `forge-test-<pid>`. One
   mutation's whole purpose is to leak cgroups, so it leaves roots behind — and
   a leftover root arrives with `cgroup.subtree_control` already set to
   `cpu memory pids`. When a later run's pid collided with one of those, the
   mutation that removes controller-enabling had nothing left to break, because
   a previous run had already enabled them. Nine such roots were on the VM,
   every one pre-configured.

**Investigation.** The decisive step was logging each mutation's test output
separately instead of overwriting one file, then reading the two that passed.
`TestCreateWritesLimits` passing under "controllers never enabled" said
`memory.max` existed in a child of a root we had definitely not configured,
which only a pre-configured root explains. Listing the leftovers and their
`subtree_control` confirmed it in one command. Before that, four rounds of
reasoning about cgroup semantics had produced nothing but plausible stories.

**Fix.** The harness aborts a case with `BUILD-FAIL` rather than reporting a
result when the mutation does not compile, and every mutation was rewritten to
compile (`n > 0` becomes `n < 0`; `attempts = 100` becomes `attempts = 1`).
Test roots come from `os.MkdirTemp`, which cannot collide with anything. The
VM was rebooted to clear the accumulated debris before the verifying run.
8 of 8 mutations are now caught, twice in a row, and the suite passes 27/27
three times running.

**Lesson.** Test isolation has to hold against the tests that are *supposed* to
break things. Per-test cleanup could never have saved this: one mutation's
entire purpose was to defeat cleanup, and the residue it left was a valid
configuration that the next experiment silently inherited. Name anything shared
by construction-unique means (`MkdirTemp`), not by something that recycles.
And a harness that reports pass/fail without checking that the thing it built
is the thing it ran is measuring its own history.

---

## 2026-09-09 — An OOM test that took 0.98s or 14.8s depending on the day

**Problem.** `TestMemoryLimitIsReportedAsAnOOMKill` and
`TestCgroupIsRemovedAfterAnOOMKill` run the same workload against a 16MiB cap.
Across two runs they took 7.15s and 0.35s, then 0.98s and 14.82s.

**Root cause.** The workload grew memory by reading a large stream into a shell
variable, `x=$(yes forge | head -c 134217728)`. `memory.max` charges page cache
to the same cgroup as anonymous memory, so with the cap that tight the kernel
reclaimed the only file-backed pages available to it — busybox's own text pages,
read from the bind-mounted rootfs — and then faulted them straight back in. The
job spent its life thrashing, and how long that took depended on the host's disk
and cache state rather than on anything the test controls.

**Investigation.** Timed both candidate workloads directly, three runs each,
before changing the test: the pipe took 653–846ms with 30% spread, while
doubling a shell variable (`x=$x$x` in a loop) took 166–195ms with 8%. Pure
anonymous memory, no reads, nothing for the kernel to reclaim except the
allocation itself.

**Fix.** Both tests use the doubling workload. Steady state is 0.14–0.19s; the
first run after a rebuild is ~3s, which is the rootfs coming back into cache.
The 15-second tail is gone.

**Lesson.** Under a memory cgroup, "allocate a lot" and "allocate a lot of
*anonymous* memory" are different experiments. Anything the job reads is
charged to the same limit and is reclaimable, so a file-backed workload measures
the kernel's reclaim heuristics and the disk, not the limit.

---

## 2026-09-09 — A skip guard that three tests ran past

**Problem.** CI failed on `make test`, the unprivileged run, with three
`mkdir /sys/fs/cgroup/forge-sandbox-test-...: permission denied` failures.
Every one of those tests passed locally, and the whole suite passed 27/27 three
times in the VM.

**Root cause.** `rootfs(t)` skips when the run is unprivileged or has no rootfs,
and every sandbox test goes through it. But three of the cgroup tests need the
root path before building the spec:

```go
root := testCgroupRoot(t)      // creates a cgroup directory
out, err := runSpec(t, Spec{Rootfs: rootfs(t), CgroupRoot: root, ...})
```

`testCgroupRoot` runs first, and it was creating a directory under
`/sys/fs/cgroup` before anything had established that the process could. A skip
guard only guards the statements after it.

**Why local runs never saw it.** `make vm-test` runs the binary under `sudo`,
where the mkdir succeeds. The failing path is the *unprivileged* one, which
locally only ever ran as part of `make test` on macOS — where these tests do not
compile at all. CI on Linux runs both, and is the only place both halves exist.

**Fix.** `testCgroupRoot` calls `rootfs(t)` itself, so the guard holds whatever
order the callers use.

**Lesson.** A helper that touches the environment has to carry its own
precondition; relying on a sibling helper being called first makes the guard
depend on statement order in every caller, forever. And the skip hazard noted in
`decisions.md` has a second face: skips make an unprivileged run green, so the
only signal that an unprivileged run is even *possible* comes from a machine
that runs both.

---

## 2026-09-09 — The missing /dev was silently writing into the shared rootfs

**Problem.** The known symptom was that `sh` prints `can't open '/dev/null'`
for every backgrounded job, because the Alpine image ships an empty `dev/`.
The symptom turned out to be the harmless half.

**Symptoms.** A redirect *succeeded*:

```
$ sudo forged -rootfs /var/tmp/forge-rootfs -- /bin/sh -c 'echo hi > /dev/null; echo wrote ok'
wrote ok
$ ls -l /var/tmp/forge-rootfs/dev/
-rw-r--r-- 1 root root 3 Sep  9 21:30 null
```

**Root cause.** With no `/dev` mounted, `dev/` is an ordinary writable
directory in the rootfs image. A shell redirect creates a regular file there.
The rootfs is bind-mounted, not copied, so that file is not scoped to the
build: it is in the image every subsequent build on this worker starts from,
growing by whatever each of them writes.

Anything *reading* a device fails outright (`dd: can't open '/dev/urandom'`),
so the failure mode is split: reads fail loudly, writes succeed and
contaminate.

**Fix.** A tmpfs on `/dev` inside the sandbox's mount namespace, with the six
standard nodes created on it. See `decisions.md` for why not a host bind mount.

**Lesson.** "The build errors out" was the reported symptom, and it framed the
whole thing as a usability gap. Reproducing each affected operation separately
— read, write, background — was what turned up the one that does not error. A
missing thing that fails loudly is a bug; a missing thing that succeeds wrongly
is a bug that spreads.

---

## 2026-09-09 — A cleanup test that made every later run fail

**Problem.** The new test asserting the rootfs image stays clean caught its own
mutations, and also caught mutations that had nothing to do with it — the
`umask` one, the wrong-minor one. Nine out of nine, for the wrong reason.

**Symptoms.** In the first mutation batch `TestDevDoesNotOutliveTheSandbox`
failed for all nine mutations. `no-tmpfs-just-nodes` additionally failed
sixteen unrelated tests, including `TestCommandRunsAsPIDOne`.

**Root cause.** Two mutations make a build write into the shared rootfs image.
The test detected that and failed — and left the file there. Every later
mutation in the batch then started from a dirty image and failed the same
assertion. The sixteen-test cascade is the same contamination one step on: a
leftover `dev/null` makes the *next* run's `mknod` fail with `EEXIST`, so the
sandbox never builds and every test in the suite fails at once.

**Fix.** The assertion clears the directory as it reads it, before and after
the run, and reports what it removed. The second batch then pins each mutation
to a specific test: `umask-not-zeroed` fails exactly
`TestDevNodesAreWritableByAnyone`, `full-has-nulls-minor` fails exactly
`TestDevNodesBehaveLikeTheirKernelDevices`.

**Lesson.** This is the third time in two weeks that a green mutation score was
the thing that needed checking rather than the thing that ended the check
(stale binaries, recycled pids, now shared host state). The tell is the same
every time: *which* test caught each mutation, not how many were caught. 9/9
and 9/9 look identical until the failing test names are read, and one of them
is a harness reporting on its own debris.

A test that asserts external state is clean has to leave it clean when it
fails. Otherwise its first failure is the only honest one.
