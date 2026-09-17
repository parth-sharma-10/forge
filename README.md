# Forge

A build/job execution platform in Go: submit a job over an API, have it
scheduled onto a worker, and run the untrusted build command inside a Linux
sandbox that Forge builds itself from namespaces, cgroups v2, `pivot_root`,
capabilities and seccomp — not Docker, not runc, not a container library.

**Status: Phases 0, 1 and 2 of 5 complete** ([docs/phase-0.md](docs/phase-0.md), [docs/phase-1.md](docs/phase-1.md), [docs/phase-2.md](docs/phase-2.md)). What exists today is the sandbox —
namespace isolation and a private device filesystem, per-job cgroups v2
resource limits, a deny-by-default seccomp filter that also bounds `clone`'s
flags, a user namespace that makes sandbox root an unprivileged uid on the
host, a per-job overlay so each build gets its own writable root filesystem,
a capability bounding set cut to what a build needs — and a worker that
claims jobs from a PostgreSQL queue with leases and fencing tokens and runs
them in it. The API and everything with more than one worker are not built
yet. This README will not claim otherwise; see [docs/architecture.md](docs/architecture.md) for what is
implemented, [docs/decisions.md](docs/decisions.md) for why, and
[docs/roadmap.md](docs/roadmap.md) for what "done" means.

## Quickstart

```sh
make test          # gofmt check, go vet, go test -race — everywhere
make test-sandbox  # the isolation tests: Linux, root, unpacks a rootfs
make vm-test       # the same, from a macOS host, inside the colima VM
make mutate        # break the sandbox 27 ways and check a test notices each
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

### The worker and the queue

A job is a row in PostgreSQL. `forge-worker` claims one with `FOR UPDATE SKIP
LOCKED`, holds a lease on it by heartbeating, runs it in the sandbox, and
writes back the exit status, whether the memory limit killed it, and the tail
of its output. The table is created on first connection; until the API lands
in Phase 3, `psql` is how a job goes in.

```
$ sudo forge-worker -dsn "$DSN" -rootfs /var/tmp/forge-rootfs -workers 2 -memory-mb 64 &
$ psql "$DSN" -c "INSERT INTO jobs (args) VALUES ('{/bin/sh,-c,\"echo building; hostname; exit 3\"}')"
INSERT 0 1
$ psql "$DSN" -c "SELECT id, state, worker, attempt, exit_code, oom_killed, convert_from(log, 'UTF8') AS log FROM jobs"
 id | state | worker | attempt | exit_code | oom_killed |   log
----+-------+--------+---------+-----------+------------+----------
  1 | done  | colima |       1 |         3 | f          | building+
    |       |        |         |           |            | forge   +
    |       |        |         |           |            |
(1 row)
```

Every claim hands out a fencing token, and every heartbeat and report names
it. A worker whose lease expired — paused, partitioned, or just slow — still
holds its copy of the job, and can even finish the build; the queue refuses
the result, because the token has moved on to whoever reclaimed it. The
worker side of that is the heartbeat: the moment it comes back "lease lost",
the build is killed, since nothing it produces can be reported.

`Ctrl-C` once drains — the jobs in hand finish, no more are claimed. Twice
kills the jobs in hand.

```
^C
level=INFO msg="signal received, draining: finishing jobs in hand, claiming no more"
level=INFO msg="worker exited cleanly"
```

## Layout

```
cmd/forge-api      HTTP API                      (not built yet)
cmd/forge-worker   job executor                  implemented
cmd/forged         sandbox runtime               implemented
internal/worker    claim, run, heartbeat, report implemented
internal/pool      bounded worker pool           implemented, unused by the worker
internal/sandbox   namespaces, pivot_root, /dev  implemented
                   seccomp allowlist             implemented
                   user namespace and id map     implemented
internal/cgroup    cgroups v2 memory/cpu/pids    implemented
internal/queue     PostgreSQL queue, leases      implemented
internal/cas       content-addressed storage     (not built yet)
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

The overlay's upper layer is a tmpfs with the kernel's default size cap, half
of RAM. A job with no memory limit can fill that much before writes fail with
`ENOSPC`; a job with one hits the limit first.

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
the report leaves a job whose lease expires and is run again; nothing
records that it happened twice. Phase 4 is where more than one worker is
tested, and where that gets measured.

There is also still no API, and no benchmark numbers — none will be quoted
here until a harness produces them.
