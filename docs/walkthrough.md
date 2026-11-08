# Walkthrough

What each layer of Forge does, shown with real commands and their output. These sections were the body of the README until 2026-11-08, with their wording unchanged. Commands that need the sandbox run on Linux as root, or inside the colima VM through `make vm-shell`.

## The sandbox (Linux, as root)

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

## Resource limits

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

## Devices

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

## Sandbox root is not host root

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

## Every job gets its own root filesystem

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

## The worker, the queue and the API

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

`GET /jobs/{id}/artifact` returns the tar of the job's `/out` once the job is
done (`404` if it left none, `409` while it runs); `artifact_bytes` on the job
says how big it is.

`POST /jobs/{id}/cancel` finishes a queued job immediately (`200`). For a
running job it answers `202`, and the worker's next heartbeat, at most two
seconds later, kills the build and reports it `cancelled`. A job that has
already finished answers `409`, and its result is kept.

`GET /jobs` lists jobs newest first, without their logs. It takes an optional
`state=` filter and a `limit=` (default 50, maximum 500), and pages with
`before=<id>`, whose value the previous page returns as `next`. Paging by id
means a job submitted between two pages doesn't shift the second one.

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

## Metrics

`forge-api`'s `/metrics` reports the queue's depth by state; `forge-worker
-metrics 127.0.0.1:9101` reports what it claimed, finished by outcome, gave
up, lost the lease on, and has in hand. Prometheus text format, written by
hand — no client library. [docs/deployment.md](deployment.md) has the
table and what each one rising means.

Add `-syscall-metrics` and each job's syscalls are counted through an eBPF
`raw_tracepoint`, attributed to the job by its cgroup, and exported as
`forge_job_syscalls_total{nr=...}` — which of the allowed syscalls builds
actually reach for. A syscall the seccomp filter denies never reaches that
tracepoint. It is counted at `sys_exit` instead, as
`forge_job_syscalls_enosys_total{nr=...}`, which is where a build probing the
filter shows up. It runs outside the sandbox, adds nothing to what a build
can do, needs `CAP_BPF`, and logs one line and stays off where it cannot load.
[docs/observability-ebpf.md](observability-ebpf.md) has the design and
what was built.
