# Deployment

What a machine needs to run Forge, and the units that run it. systemd,
not a container: the sandbox needs a real kernel and its own cgroup subtree,
and a container runtime is the thing Forge was built not to need.

## The machine

- Linux 5.7 or newer with cgroups v2 mounted at `/sys/fs/cgroup` (any
  current distribution; Ubuntu 22.04+, Debian 12+, Fedora). Kernel 6.8 is
  what CI and the development VM run.
- A fully visible `/proc`. A worker inside another container usually does
  not have one, and the sandbox reports that as `EPERM`.
- PostgreSQL, reachable from the API and every worker; tested on 16 and
  18, and nothing in the queries is newer than 9.5. The table is created by
  whichever connects first.
- Go 1.27 to build: `make build`, then `cmd/forge-api` and
  `cmd/forge-worker` go to `/usr/local/bin`. There is nothing else to
  install; the binaries are static.

## The image

```sh
sudo mkdir -p /var/lib/forge
sudo ROOTFS=/var/lib/forge/rootfs make rootfs
```

`make rootfs` unpacks Alpine, pinned by SHA-256 in the Makefile, and shifts
every file's owner into the sandbox's uid map — uid 0 in the image becomes
524288 on the host. The shift is what lets the per-job overlay copy files
up; an image unpacked by hand needs `sudo hack/shift-rootfs.sh <path>` once.
The worker refuses nothing here, but every build on an unshifted image
fails its first write with `EOVERFLOW`.

Any image will do, not only Alpine: unpack it, shift it, point `-rootfs`
at it. Whatever the jobs run has to already be in it — the sandbox has no
network, so nothing can be fetched from inside.

## The units

`deploy/forge-api.service` and `deploy/forge-worker.service`. Install them
to `/etc/systemd/system/`, write the two environment files, and enable.

```sh
sudo useradd --system --home /nonexistent --shell /usr/sbin/nologin forge
sudo install -d -m 0750 -o root -g forge /etc/forge
echo 'FORGE_DSN=postgres://forge:...@db.internal/forge' | sudo tee /etc/forge/api.env /etc/forge/worker.env >/dev/null
echo "FORGE_API_TOKEN=$(openssl rand -hex 32)" | sudo tee -a /etc/forge/api.env >/dev/null
sudo chmod 0640 /etc/forge/*.env
sudo cp deploy/forge-api.service deploy/forge-worker.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now forge-api forge-worker
```

**The worker runs as root.** Building the sandbox — the clone into seven
namespaces, the mounts, `pivot_root` — needs `CAP_SYS_ADMIN` on the host
side. The build inside runs as uid 524288 with nine capabilities and a
syscall allowlist; the README's *Limitations* say exactly what that bounds.

**`Delegate=yes`** on the worker's unit is not optional. It hands the
unit's cgroup subtree to the worker, which creates one cgroup per job under
`-cgroup-root`; without delegation systemd owns the subtree and the first
job fails at `cgroup.Create`. The unit's `-cgroup-root` names its own
subtree, `/sys/fs/cgroup/system.slice/forge-worker.service`.

**The API runs as `forge`**, unprivileged, on loopback. Every route,
`/metrics` included, wants `Authorization: Bearer $FORGE_API_TOKEN`, and
`forge-api` will not start without the token — an `api.env` holding only
`FORGE_DSN` leaves the unit restart-looping on exit status 2. The token is
one shared secret over plain HTTP, so anything past loopback wants TLS in
front; a proxy that authenticates on its own can run the API with
`-insecure-no-auth` instead. Never on an interface a build can reach.

**Stopping.** `systemctl stop forge-worker` sends `SIGTERM`, which drains:
the jobs in hand finish, no more are claimed. `TimeoutStopSec=300` is how
long a build gets; past it systemd's `SIGKILL` takes the worker and, through
the sandbox's `Pdeathsig`, every build with it. Their leases expire and
another worker reclaims them.

## Sizing

- `-workers` is how many jobs one machine runs at once; `-memory-mb` and
  `-pids` are per job. The unit ships `4`, `2048`, `512`.
- `-lease` (30s) is how long a dead worker's jobs wait before another
  reclaims them. Shorter reclaims faster and heartbeats more often.
- `-max-attempts` (3) is how many workers a job may kill before it is
  given up.

## Metrics

Both binaries speak the Prometheus text format; scrape them.

| Endpoint | Metric | Meaning |
|---|---|---|
| API `/metrics` | `forge_queue_jobs{state}` | jobs queued, running, done — the depth |
| worker `-metrics addr` | `forge_jobs_claimed_total` | claims this worker made |
| | `forge_jobs_done_total{outcome}` | `ok`, `failed`, `oom`, `killed`, `setup`, `given_up` |
| | `forge_lease_lost_total` | heartbeats refused: a job this worker held was reclaimed |
| | `forge_jobs_running` | jobs in hand now |

`forge_lease_lost_total` rising on a healthy worker means `-lease` is
shorter than a heartbeat can reliably be delivered — a database or network
that stalls for longer than a third of the lease. `given_up` rising means
jobs are killing workers, or `-max-attempts` is too low for the failures
that happen anyway.

## The table

Every job is one row, updated on claim, on every heartbeat, and on
report — several dead tuples per job. Measured on the development VM: the
benchmark ran at 25–229 ms per job on a table carrying the rows of every
earlier run, and at 10–18 ms once the table was emptied, which is why the
benchmark now empties it. Dead tuples and stale statistics are the usual
reasons a query behaves like that, and autovacuum is what handles both;
the default thresholds are set for tables that change less than this one.
Two things help, and the second is the one to do:

- lower the autovacuum thresholds on `jobs` —
  `ALTER TABLE jobs SET (autovacuum_vacuum_scale_factor = 0.01,
  autovacuum_vacuum_cost_delay = 0)`;
- delete or archive `done` rows on a schedule. Nothing in Forge reads a
  done row again except `GET /jobs/{id}`; how long that has to work is a
  retention decision Forge does not make for you.
