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
