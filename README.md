# Forge

A build/job execution platform in Go: submit a job over an API, have it
scheduled onto a worker, and run the untrusted build command inside a Linux
sandbox that Forge builds itself from namespaces, cgroups v2, `pivot_root`,
capabilities and seccomp — not Docker, not runc, not a container library.

**Status: Phase 0, Week 1 of 5 phases.** What exists today is the bounded worker
pool and the project's CI. The sandbox, API, queue, and everything distributed
are not built yet. This README will not claim otherwise; see
[docs/architecture.md](docs/architecture.md) for what is implemented and
[docs/decisions.md](docs/decisions.md) for why.

## Quickstart

```sh
make test          # gofmt check, go vet, go test -race
go run ./cmd/forge-worker
```

`forge-worker` is currently a driver for the pool. Press `Ctrl-C` once and it
drains — no new tasks accepted, accepted ones finish. Press it twice and running
tasks get their context cancelled.

```
$ go run ./cmd/forge-worker -workers=2 -queue=2 -tasks=100 -task-duration=1s
level=INFO msg="task started" task=1
level=INFO msg="task started" task=0
^C
level=INFO msg="signal received, draining: no new work accepted"
level=INFO msg="stopped submitting" submitted=4 reason="pool: closed"
level=INFO msg="task done" task=1
level=INFO msg="task started" task=2          # queued before the drain, still runs
^C
level=WARN msg="second signal, cancelling work in flight"
level=INFO msg="task cancelled" task=0
level=INFO msg="task cancelled" task=2
level=INFO msg="worker exited cleanly"
```

## Layout

```
cmd/forge-api      HTTP API                      (not built yet)
cmd/forge-worker   job executor                  (pool driver today)
cmd/forged         sandbox runtime               (not built yet)
internal/pool      bounded worker pool           implemented
internal/sandbox   namespaces, cgroups, seccomp  (not built yet)
internal/queue     PostgreSQL queue, leases      (not built yet)
internal/cas       content-addressed storage     (not built yet)
docs/              architecture, decisions, failures
```

## Requirements

Go 1.27+. The sandbox work from Week 2 onward requires a Linux kernel with
cgroups v2; those packages are behind `//go:build linux` and run in CI on
`ubuntu-latest`.

## Limitations

Everything, at this stage. There is no API, no persistence, no isolation, and no
benchmark numbers — and none will be quoted here until a harness produces them.
