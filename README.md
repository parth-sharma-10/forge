# Forge

A build/job execution platform in Go: submit a job over an API, have it
scheduled onto a worker, and run the untrusted build command inside a Linux
sandbox that Forge builds itself from namespaces, cgroups v2, `pivot_root`,
capabilities and seccomp — not Docker, not runc, not a container library.

**Status: Phase 0, Week 3 of 5 phases.** What exists today is the bounded worker
pool, the sandbox's namespace isolation, and per-job cgroups v2 resource limits.
The API, the queue and everything distributed are not built yet. This README will not claim
otherwise; see [docs/architecture.md](docs/architecture.md) for what is
implemented and [docs/decisions.md](docs/decisions.md) for why.

## Quickstart

```sh
make test          # gofmt check, go vet, go test -race — everywhere
make test-sandbox  # the isolation tests: Linux, root, unpacks a rootfs
make vm-test       # the same, from a macOS host, inside the colima VM
```

### The sandbox (Linux, as root)

```sh
make rootfs                                    # Alpine, pinned by SHA-256
go build -o /tmp/forged ./cmd/forged
sudo /tmp/forged -rootfs /var/tmp/forge-rootfs -- /bin/sh -c 'hostname; echo $$; ls /'
```

```
forge
1
bin
dev
etc
...
usr
var
```

PID 1, its own hostname, and a root filesystem with no path back to the host's.
The command's exit status is passed through:

```
$ sudo /tmp/forged -rootfs /var/tmp/forge-rootfs -- /bin/sh -c 'exit 7'; echo $?
7
$ sudo /tmp/forged -rootfs /tmp -- /bin/sh -c true; echo $?
sandbox: exec: "/bin/sh": stat /bin/sh: no such file or directory
125
```

125 is reserved for the sandbox failing to build, so a scheduler can tell "this
build fails" from "this worker cannot run builds".

### Resource limits

Each job gets its own cgroup, and the kernel puts the build in it at clone time
— there is no window in which it runs uncapped. Limits are opt-in; the default
is unlimited.

```sh
sudo /tmp/forged -rootfs /var/tmp/forge-rootfs \
  -memory-mb 64 -cpu 0.5 -pids 32 -- /bin/sh -c 'echo built ok'
```

Exceeding the memory limit is reported as what it is, rather than left to look
like every other SIGKILL:

```
$ sudo /tmp/forged -rootfs /var/tmp/forge-rootfs -memory-mb 16 \
    -- /bin/sh -c 'x=aaaaaaaaaaaaaaaa; while :; do x=$x$x; done'
sandbox: killed by the memory limit: signal: killed
$ echo $?
137
```

A lease timeout, an operator's `kill -9` and an OOM kill are all exit 137, and
the scheduler's decision differs for each — so Forge reads `memory.events`
rather than guessing from the status. `-cpu` throttles instead of killing;
`-pids` makes the offending `fork` fail (`can't fork: Resource temporarily
unavailable`) rather than letting a runaway `make -j` take the worker down.

### The worker pool

`forge-worker` is currently a driver for the pool. Press `Ctrl-C` once and it
drains — no new tasks accepted, accepted ones finish. Press it twice and running
tasks get their context cancelled.

```
$ go run ./cmd/forge-worker -workers=2 -queue=2 -tasks=100 -task-duration=1s
level=INFO msg="task started" task=1
^C
level=INFO msg="signal received, draining: no new work accepted"
level=INFO msg="task done" task=1
^C
level=WARN msg="second signal, cancelling work in flight"
level=INFO msg="worker exited cleanly"
```

## Layout

```
cmd/forge-api      HTTP API                      (not built yet)
cmd/forge-worker   job executor                  (pool driver today)
cmd/forged         sandbox runtime               implemented
internal/pool      bounded worker pool           implemented
internal/sandbox   namespaces, pivot_root        implemented
                   seccomp                       (not built yet)
internal/cgroup    cgroups v2 memory/cpu/pids    implemented
internal/queue     PostgreSQL queue, leases      (not built yet)
internal/cas       content-addressed storage     (not built yet)
docs/              architecture, decisions, failures
```

## Requirements

Go 1.27+, and no third-party dependencies — `go.mod` has no `require` block.

The sandbox needs `CAP_SYS_ADMIN` and a Linux kernel with cgroups v2 and
`CLONE_INTO_CGROUP` — 5.7 or newer. Those packages are behind `//go:build
linux`; CI runs their tests on `ubuntu-latest`, and a macOS host runs them
through `make vm-test` against a colima VM.

## Limitations

The sandbox isolates against *accident*, not against a hostile build. There is
no user namespace yet, so root inside the sandbox is root on the host, and no
seccomp filter, so the full syscall surface is reachable.

There is also still no `/dev`, which is more disruptive than it sounds: `sh`
opens `/dev/null` for every backgrounded job, so a build script containing `&`
prints `can't open '/dev/null'` for each one. A tmpfs `/dev` with the standard
nodes is the next thing the sandbox needs to run real builds.

There is also still no API, no persistence, and no benchmark numbers — and none
will be quoted here until a harness produces them.
