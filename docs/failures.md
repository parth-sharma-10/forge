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
