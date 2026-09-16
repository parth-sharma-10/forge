# Phase 5 — operable

The last phase. Everything Forge does, it now does; this phase is what an
operator needs to run it and know it is running: numbers from the process,
a document that gets it installed, and the benchmark numbers the README
has refused to quote until a harness produced them.

## Deliverables

| # | Deliverable | Proven by |
|---|---|---|
| 1 | `/metrics` in the Prometheus text format, written by hand — no client library, the dependency count stays at one. The API exposes the queue's depth by state; the worker exposes what it has claimed, finished by outcome, given up, and lost the lease on, and how many jobs it is running now | `internal/metrics` tests for the format; `TestMetricsCountTheQueue` (api), `TestMetricsCountTheWork` (worker) |
| 2 | `docs/deployment.md`: what to install, the systemd units, the cgroup delegation the README already names, the image and its shift, the database, and where the API belongs on the network | `deploy/forge-api.service` and `deploy/forge-worker.service` pass `systemd-analyze verify` in CI |
| 3 | Benchmarks in the repo: sandbox setup latency (`BenchmarkRun`, root + image) and jobs per second through the queue (`BenchmarkThroughput`, root + image + database), run by `make bench` in the VM and `make bench` in CI | numbers in the README, with the machine they came from |

## Decisions made here, recorded in decisions.md

- **The text format by hand.** A counter is a name, a help line, a type
  line and a number; the client library is for histograms and pushes,
  and Forge has neither.
- **Benchmarks are `go test -bench`.** The standard harness, in the
  packages the code lives in, run the way the tests are run. A benchmark
  that needs root and an image skips without them, like the tests do.
- **Numbers are quoted with their machine.** A number without the
  hardware it came from is a claim, not a measurement.

## Not in Phase 5

- Histograms, tracing, structured log shipping. Counters and a gauge are
  what a first alert needs.
- A Helm chart, a container image. The deployment document is systemd,
  because the sandbox needs a real kernel and cgroup delegation, and a
  container runtime is the thing Forge was built not to need.
- Authentication for the API or `/metrics`. Both trust their network.

## Exit criteria

All four held on 2026-09-17 at the commit this file was last changed in.

1. **CI green on x86_64**: metrics tests running, `make verify-units`
   passing with the binaries installed, `make bench` logging its numbers
   after the privileged suite's 72 passes, 0 skips, 0 failures.
2. **The numbers the README quotes are from a harness in the repo** — from
   CI's `bench` step rather than the VM, which was found to be hosting
   another project's cluster at load 169; `docs/failures.md` has the
   account, and the VM's numbers are given beside CI's for contrast.
3. **`go.mod` requires exactly one module.**
4. **`docs/roadmap.md` says five of five**, and the README's first sentence
   is true: a job submitted over the API is claimed by a worker and runs in
   the sandbox, and a worker that dies mid-job does not lose it.
