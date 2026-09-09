# Forge

A build/job execution platform in Go: submit a job over an API, have it
scheduled onto a worker, and run the untrusted build command inside a Linux
sandbox that Forge builds itself from namespaces, cgroups v2, `pivot_root`,
capabilities and seccomp — not Docker, not runc, not a container library.

**Status: Phase 0, Week 2 of 5 phases.** What exists today is the bounded worker
pool and the sandbox's namespace isolation. The API, the queue, resource limits
and everything distributed are not built yet. This README will not claim
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
                   cgroups, seccomp              (not built yet)
internal/queue     PostgreSQL queue, leases      (not built yet)
internal/cas       content-addressed storage     (not built yet)
docs/              architecture, decisions, failures
```

## Requirements

Go 1.27+, and no third-party dependencies — `go.mod` has no `require` block.

The sandbox needs a Linux kernel with cgroups v2 and `CAP_SYS_ADMIN`. Its
package is behind `//go:build linux`; CI runs those tests on `ubuntu-latest`,
and a macOS host runs them through `make vm-test` against a colima VM.

## Limitations

The sandbox isolates against *accident*, not against a hostile build. There is
no user namespace yet, so root inside the sandbox is root on the host; there are
no cgroup limits, so a build can exhaust the machine's memory and CPU; there is
no seccomp filter, so the full syscall surface is reachable; and there is no
`/dev`, so a build that writes to `/dev/null` fails.

There is also still no API, no persistence, and no benchmark numbers — and none
will be quoted here until a harness produces them.
