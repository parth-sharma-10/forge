# Forge

A build/job execution platform in Go: submit a job over an API, have it
scheduled onto a worker, and run the untrusted build command inside a Linux
sandbox that Forge builds itself from namespaces, cgroups v2, `pivot_root`,
capabilities and seccomp — not Docker, not runc, not a container library.

**Status: all five phases complete** ([docs/phase-0.md](docs/phase-0.md) through [docs/phase-5.md](docs/phase-5.md); [docs/deployment.md](docs/deployment.md) to run it). What exists is the sandbox —
namespace isolation and a private device filesystem, per-job cgroups v2
resource limits, a deny-by-default seccomp filter that also bounds `clone`'s
flags, a user namespace that makes sandbox root an unprivileged uid on the
host, a per-job overlay so each build gets its own writable root filesystem,
a capability bounding set cut to what a build needs — a worker that claims
jobs from a PostgreSQL queue with leases and fencing tokens and runs them in
it, and an HTTP API that submits jobs, reports them and streams their logs.
More than one worker against the same queue is tested by killing one
mid-job. Both binaries expose Prometheus metrics, the deployment document
has the systemd units, and the benchmark numbers below come from a harness
in the repo. This README will not claim otherwise; see [docs/architecture.md](docs/architecture.md) for what is
implemented, [docs/decisions.md](docs/decisions.md) for why, and
[docs/roadmap.md](docs/roadmap.md) for what "done" means.

## Quickstart

```sh
make test          # gofmt check, go vet, go test -race — everywhere
make test-sandbox  # the isolation tests: Linux, root, unpacks a rootfs
make vm-test       # the same, from a macOS host, inside the colima VM
make mutate        # break the sandbox 27 ways and check a test notices each
make mutate-queue  # break the queue 4 ways, on the host, against FORGE_TEST_DSN
make fuzz          # fuzz the seccomp filter against its policy (Linux, Go, no root)
make bench         # sandbox setup latency and jobs per second (Linux, root)
make vm-bench      # the same, inside the colima VM
```

The queue's tests run against a real PostgreSQL named by `FORGE_TEST_DSN`
(`postgres://forge@127.0.0.1:5432/postgres`); they skip without one on a
developer's machine and fail without one in CI. `make vm-test` expects one
inside the VM at `VM_DSN`.

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

### Devices

The sandbox gets a `/dev` of its own — a tmpfs holding six character devices
and four symlinks, and nothing else, so a build cannot reach `/dev/sda` or
`/dev/mem` through it. The six are bind mounts of the host's nodes, each one
checked against the kernel's major and minor numbers before it is bound.

```
$ sudo /tmp/forged -rootfs /var/tmp/forge-rootfs -- /bin/sh -c 'ls /dev; sleep 0 & wait; echo ok'
fd
full
null
random
stderr
stdin
stdout
tty
urandom
zero
ok
```

Without it, `sh` printed `can't open '/dev/null'` for every backgrounded job,
`dd if=/dev/urandom` failed outright — and, worse, `echo hi > /dev/null`
*succeeded*, leaving a regular file called `null` in the rootfs image that
every later build on the worker inherited.

### Sandbox root is not host root

The build is uid 0 inside its user namespace and uid 524288 outside it. The
image is shifted into that map by `make rootfs` (`hack/shift-rootfs.sh`), so
from inside it belongs to root the way any container image does; host root
itself is not mapped and reads as nobody.

```
$ sudo /tmp/forged -rootfs /var/tmp/forge-rootfs -- /bin/sh -c \
    'id; cat /proc/self/uid_map; stat -c "%u %g %n" /bin/busybox'
uid=0(root) gid=0(root) groups=65534(nobody)
         0     524288      65536
0 0 /bin/busybox
```

The single supplementary group is the overflow gid — host root's group, which
the map does not cover either. `setgroups` is denied inside the namespace, so a
build cannot trade it for one that names something.

### Every job gets its own root filesystem

The image is the lower layer of an overlay; the upper layer is a tmpfs the
sandbox mounts inside its own mount namespace. A build can write anywhere —
install packages, edit `/etc`, delete from `/usr` — and the image on the host
is untouched. When the job exits, its namespace goes and the layer with it:
there is no per-job directory on the host and nothing to sweep.

```
$ sudo /tmp/forged -rootfs /var/tmp/forge-rootfs -- /bin/sh -c \
    'apk del --quiet libc-utils; rm /etc/motd; echo built > /usr/out; ls /usr/out /usr/bin/getconf /etc/motd'
ls: /etc/motd: No such file or directory
ls: /usr/bin/getconf: No such file or directory
/usr/out

$ ls /var/tmp/forge-rootfs/usr/out /var/tmp/forge-rootfs/usr/bin/getconf /var/tmp/forge-rootfs/etc/motd
ls: cannot access '/var/tmp/forge-rootfs/usr/out': No such file or directory
/var/tmp/forge-rootfs/etc/motd
/var/tmp/forge-rootfs/usr/bin/getconf
```

The layer's pages are charged to the job's cgroup like the rest of its memory,
so `-memory-mb` bounds it too; a job that writes 64 MiB into `/usr` under a
16 MiB limit is OOM-killed the same way one that allocates 64 MiB is.

### The worker, the queue and the API

A job is a row in PostgreSQL. `forge-worker` claims one with `FOR UPDATE SKIP
LOCKED`, holds a lease on it by heartbeating, runs it in the sandbox, and
writes back the exit status, whether the memory limit killed it, and the tail
of its output — with every heartbeat while it runs, so the log can be
followed. `forge-api` is the front door: it needs no privileges and never
touches the sandbox. The table is created by whichever connects first.

```
$ export FORGE_API_TOKEN=$(openssl rand -hex 32)
$ forge-api -dsn "$DSN" -listen 127.0.0.1:18080 &
$ sudo forge-worker -dsn "$DSN" -rootfs /var/tmp/forge-rootfs -workers 2 -memory-mb 64 &
$ auth="Authorization: Bearer $FORGE_API_TOKEN"
$ curl -s -H "$auth" -X POST 127.0.0.1:18080/jobs -d '{"args": ["/bin/sh", "-c", "for i in 1 2 3; do echo step $i; sleep 1; done; exit 3"]}'
{"id":1}
$ curl -sN -H "$auth" 127.0.0.1:18080/jobs/1/logs        # streams; each line arrived a second apart
step 1
step 2
step 3
$ curl -s -H "$auth" 127.0.0.1:18080/jobs/1
{"id":1,"state":"done","attempt":1,"worker":"colima","exit_code":3,"oom_killed":false,"log_bytes":21}
```

`GET /jobs/{id}/logs` follows a running job by re-reading its row every half
second and writing what is new, and returns at once for a job that is done.
The row holds the last megabyte of output; a follower that missed more than
that is told how many bytes, rather than shown a splice. Every route,
`/metrics` included, wants `Authorization: Bearer <token>`, compared in
constant time; `forge-api` refuses to start without a token unless told
`-insecure-no-auth`, for a proxy that authenticates in front of it.

Every claim hands out a fencing token, and every heartbeat and report names
it. A worker whose lease expired — paused, partitioned, or just slow — still
holds its copy of the job, and can even finish the build; the queue refuses
the result, because the token has moved on to whoever reclaimed it. The
worker side of that is the heartbeat: the moment it comes back "lease lost",
the build is killed, since nothing it produces can be reported.

A worker that dies mid-job — `SIGKILL`, a lost machine — leaves a job
whose lease expires within `-lease` and which any other worker then
reclaims and runs again; the row's `attempt` says how many times that
happened. A worker that merely stopped heartbeating and finishes late finds
its report refused: the fencing token moved on with the reclaim. A job
claimed more than `-max-attempts` times (three by default) is reported as
given up rather than run again, so a job that kills every worker it lands
on stops landing.

`Ctrl-C` once drains — the jobs in hand finish, no more are claimed. Twice
kills the jobs in hand.

### Metrics

`forge-api`'s `/metrics` reports the queue's depth by state; `forge-worker
-metrics 127.0.0.1:9101` reports what it claimed, finished by outcome, gave
up, lost the lease on, and has in hand. Prometheus text format, written by
hand — no client library. [docs/deployment.md](docs/deployment.md) has the
table and what each one rising means.

Add `-syscall-metrics` and each job's syscalls are counted through an eBPF
`raw_tracepoint`, attributed to the job by its cgroup, and exported as
`forge_job_syscalls_total{nr=...}` — which of the allowed syscalls builds
actually reach for. It runs outside the sandbox, adds nothing to what a build
can do, needs `CAP_BPF`, and logs one line and stays off where it cannot load.
[docs/observability-ebpf.md](docs/observability-ebpf.md) has the design and
what was built.

### Numbers

From `make bench` in CI on a GitHub-hosted `ubuntu-latest` runner (x86_64,
`GOMAXPROCS` 2 as Go saw it, PostgreSQL 17 in a service container,
the Alpine image), one run of twenty iterations — the log of
[PR #15](https://github.com/parth-sharma-10/forge/pull/15)'s `bench` step.

| Benchmark | What it measures | Result |
|---|---|---|
| `BenchmarkRun` | one sandbox: six namespaces, overlay, `/proc`, `/dev`, `pivot_root`, bounding set, seccomp, `exec /bin/true`, teardown | 5.9 ms |
| `BenchmarkRunWithLimits` | the same with a cgroup per job | 5.8 ms |
| `BenchmarkThroughput` | jobs per second through the queue and the sandbox, four loops, `/bin/true` | 10.1 ms per job — about 100 jobs/s |

The same benchmarks on the development VM — colima, 2 vCPUs, 4 GB, kernel
6.8 aarch64 — read 11–39 ms per sandbox and 10–18 ms per job, and swung
threefold between runs. The VM turned out to be hosting another project's
Kubernetes cluster at a load average of 169; the numbers above are from
the machine that was not. The throughput benchmark also first measured
PostgreSQL rather than Forge — see [docs/deployment.md](docs/deployment.md),
*The table*.

## Layout

```
cmd/forge-api      HTTP API                      implemented
cmd/forge-worker   job executor                  implemented
internal/api       POST /jobs, GET, log streaming implemented
internal/metrics   Prometheus text format         implemented
deploy/            systemd units                  verified in CI
cmd/forged         sandbox runtime               implemented
internal/worker    claim, run, heartbeat, report implemented
internal/pool      bounded worker pool           implemented, unused by the worker
internal/sandbox   namespaces, pivot_root, /dev  implemented
                   seccomp allowlist             implemented
                   user namespace and id map     implemented
internal/cgroup    cgroups v2 memory/cpu/pids    implemented
internal/queue     PostgreSQL queue, leases      implemented
docs/              architecture, decisions, failures
```

## Requirements

Go 1.27+, PostgreSQL for the queue, and one third-party dependency: `pgx/v5`,
its driver.

The sandbox needs `CAP_SYS_ADMIN` and a Linux kernel with cgroups v2 and
`CLONE_INTO_CGROUP` — 5.7 or newer. It re-executes itself after dropping to the
mapped uid, so the Forge binary has to be readable and executable by that uid:
a worker installed mode `0700` starts nothing.

It also needs a fully visible `/proc` on the host, which is what a worker
running inside another container usually does not have — a masked `/proc`
leaves the sandbox unable to mount one of its own, and the kernel reports that
as `EPERM` rather than as what it is. Those packages are behind `//go:build
linux`; CI runs their tests on `ubuntu-latest`, and a macOS host runs them
through `make vm-test` against a colima VM.

## Limitations

Every job on a worker is mapped to the same host uid range. Since the overlay,
no two jobs share a writable filesystem, so what remains of that is on the
host's side: everything any job creates outside its layer — nothing, today —
would belong to the same ids.

The overlay's upper layer is a tmpfs. `-disk-mb` caps it, bytes and inodes
both, so a build that fills it gets `ENOSPC` rather than an OOM kill; without
the flag it has the kernel's default cap, half of RAM, and a job's memory limit
is what stops it first. `-timeout` (an hour by default) kills a build that runs
past it and reports it as `timeout`.

Nothing a build writes survives the job. Artifacts come out on stdout or not
at all until a later phase gives the worker a way to collect them.

The seccomp filter is an allowlist of syscall *numbers*, with one exception:
`clone` is also checked for `CLONE_NEW*` flags, and refused with `EPERM` when
it carries any, so a build can fork and thread but not create namespaces of
its own. `clone3` is denied by number, since its flags are in a struct the
filter cannot read; glibc falls back to `clone`. An unlisted syscall returns
`ENOSYS`, which is what lets a C library fall back to an older call rather
than fail outright — and which makes a denial read like an old kernel in a
build log rather than like a denial.

The `/dev` the sandbox provides is six nodes and four symlinks, and nothing
else. A build that wants a pty or POSIX shared memory (`/dev/ptmx`,
`/dev/shm`) does not get one yet. Making new device nodes is denied twice over:
the filter answers `mknod` with `ENOSYS`, and the user namespace would refuse
it anyway.

A build holds nine capabilities after `execve` — chown, dac_override, fowner,
fsetid, kill, setgid, setuid, net_bind_service, setfcap — and no others, in
any set. `CAP_SYS_ADMIN` is gone before the build starts. The bounding set is
what enforces it, and the set only shrinks; `CAP_SETPCAP`, which shrinks it,
is not among the nine. The kept list is decided by what the tests show a
package manager doing, not by what a build might someday want.

Delivery is at-least-once. A worker that dies after the build and before
the report leaves a job whose lease expires and is run again; the row's
attempt count says it happened, and nothing makes the second run unnecessary
— idempotent jobs are the caller's business.

The API's authentication is one shared bearer token, not users or roles. It
has no way to cancel or list jobs, and no
webhook — a webhook is a client of `POST /jobs` and can live anywhere.

The benchmark numbers above are from one small VM. They are what the
harness produced there and nothing more.

[docs/threat-model.md](docs/threat-model.md) is the whole of this — what each
boundary is, the code and test that hold it, and what Forge does not defend
against at all, the largest being a kernel exploit through an allowed syscall.
