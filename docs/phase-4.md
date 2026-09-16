# Phase 4 — more than one worker

Phase 2 wrote the queue for several workers and tested it with one. This
phase is where the design is tested rather than described: workers are
killed mid-job, leases expire, and the fencing token has to do the thing it
exists for. Written before the code, as the phases before it were.

## Deliverables

| # | Deliverable | Proven by |
|---|---|---|
| 1 | A job survives its worker: a worker process `SIGKILL`ed mid-job leaves a job whose lease expires, and another worker claims it, runs it again and reports it | `TestAJobSurvivesItsWorker` — a real second process, killed with `SIGKILL`, the sandbox's `Pdeathsig` taking the build with it |
| 2 | A late worker's report is harmless: a worker that stopped heartbeating, lost its lease, and then finished the build cannot overwrite the result of the worker that took the job over | `TestALateWorkersReportIsRefused` |
| 3 | A job is never run twice *at once*: while one worker's lease is live, no other worker can claim the job, however many try | `TestALiveLeaseIsExclusive` |
| 4 | An attempt cap: a job that has been claimed `MaxAttempts` times is reported as given up rather than run again, so a job that kills every worker it lands on stops landing | `TestAJobIsGivenUpAfterMaxAttempts` |
| 5 | At-least-once is measured: the number of attempts and which worker ran each is on the row and visible in the API, and the README says what it means | `TestAJobSurvivesItsWorker` asserts attempt 2 |
| 6 | Mutations for the queue: the fence check dropped from `Report`, the reclaim clause dropped from `Claim`, the advisory lock dropped from `Open` — each caught | `make mutate-queue`, run on the host against `FORGE_TEST_DSN` |

## Decisions made here, recorded in decisions.md

- **The attempt cap is enforced by the worker, not the claim query.** The
  claim hands the job out with its attempt number; a worker handed an
  attempt past the cap reports it as given up without running it. The
  alternative — excluding such jobs in `Claim`'s `WHERE` — leaves them
  `running` forever with an expired lease, which is a row nothing will
  ever touch again and no state name for it.
- **Death is tested with a real process.** A goroutine cannot be killed;
  a worker can. The test binary gains a fourth identity — a worker with a
  fake runner, driven by a job's args — and the test starts it, watches it
  claim, and `SIGKILL`s it.
- **The queue mutations run on the host**, not in the VM: they need
  PostgreSQL and no privileges. `hack/mutate.py` learns a second target.

## Not in Phase 4

- Exactly-once. A worker that dies between the build's end and its report
  leaves a job that runs again; the row records both attempts, and that is
  the honest contract. Idempotent jobs are the caller's business.
- A `runs` table. Attempt count and last worker on the job row cover what
  the tests need; a history per attempt is not asked for by anything.
- Cancelling a job from the API.

## Exit criteria

1. CI green on x86_64 with the multi-worker tests running against the
   service container, none skipped.
2. `make mutate` still 27/27; `make mutate-queue` exits 0.
3. `go.mod` still requires exactly one module.
