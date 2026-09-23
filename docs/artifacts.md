# Artifacts — a design, not yet a feature

Status: **designed, not built.** Nothing a build writes survives the job; the
only output is stdout and stderr, through a 1 MiB log tail. Collecting files is
the largest product gap left, and it is a security decision before it is a
feature: [threat-model.md](threat-model.md) names artifact collection as a
review trigger, because it is the first way a build's writes could leave its
layer. The decision is the owner's; this document gives the options and the
recommendation.

## The constraint

The writable layer is a tmpfs mounted inside the job's own mount namespace
(`mountOverlay`, [decisions.md](decisions.md) 2026-09-11). When PID 1 exits, the
namespace goes and the layer with it. Nothing on the host can reach it, before
or after. That is the property that makes every job's filesystem private
(boundary 3), and any collection scheme has to either keep it or say what it
trades for it.

## Options

### A. Put the upper layer on a host directory and collect from it afterwards

This is what Phase 1 deferred. The worker creates a per-job directory, the
overlay's `upperdir` goes there, and after the build exits the worker walks
`upper/out` and stores what it finds.

The problem is that the worker would be walking an attacker-built tree **on
the host, as root**:
- **Symlinks** to `/etc/shadow` or `/var/lib/forge/…`: any walk that follows
  one reads host files into the artifact.
- **Special files.** `mknod` is denied today, so a build can't create a FIFO
  whose `open` would block the worker. That is a seccomp policy the
  collector would be quietly relying on, though. `bind` is allowed, so unix
  socket files can appear. The walker has to take regular files only, with
  `O_NONBLOCK`, whatever the filter says.
- **Overlay whiteouts** appear on the host as character devices 0:0.
- **Size.** A sparse file with a large apparent size, a million empty files,
  and path depth all need their own caps.
- **Cleanup.** A worker that crashes leaves per-job directories that something
  has to sweep. The in-namespace tmpfs has no such state today.
- **uid ranges.** Files on the host are owned by the shared mapped range, which
  makes *jobs share a uid range* (threat model) matter for the first time.

Every item here can be handled (`openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS)`,
`O_NONBLOCK`, regular files only, a size-capped host tmpfs). But each is a
place to get it wrong, in root code on the host.

### B. A supervisor PID 1 that streams a tar out over a pipe — recommended

The re-executed child stops exec'ing the build over itself. It stays as PID 1,
a small init of Forge's own, and:

1. forks the build and execs it with stdio only. The artifact pipe is
   `O_CLOEXEC`, so `TestNoHostDescriptorLeaksIntoTheBuild` still holds as
   written;
2. waits for it, reaping the orphaned processes as they come (which the build
   as PID 1 does not do today);
3. once the build exits, `kill(-1, SIGKILL)` removes everything left in the PID
   namespace, so **nothing of the build is running while `/out` is read**. That
   removes every TOCTOU race at once;
4. walks `/out` with `lstat`: regular files and directories only, symlinks
   recorded as links and never followed, everything else skipped. It writes a
   tar to the pipe, stopping at a byte cap;
5. exits with the build's status.

The worker reads the pipe through an `io.LimitReader` and stores the result.
It never touches the build's filesystem, the layer still dies with the
namespace, and nothing is created on the host. The attacker-controlled walk
happens *inside* the sandbox: namespaces, the unprivileged uid, a dropped
bounding set and a seccomp filter apply to it. The supervisor drops the
bounding set and installs the filter on itself *before* it forks the build,
which inherits both. Everything it does afterwards (`wait4`, `kill`, `openat`,
`getdents64`, `newfstatat`, `read`, `write`) is already on the allowlist.

Costs:
- The build is no longer PID 1. `TestCommandRunsAsPIDOne` becomes "the build's
  parent is Forge's PID 1". Signal delivery for cancel and timeout still works,
  because killing PID 1 of a PID namespace kills the namespace.
- Exit status has to be passed through the supervisor, including the
  signal-killed and OOM cases the worker already distinguishes.
- The supervisor is new code that runs in every job. It stays small: fork,
  wait, kill, walk, tar. The walk uses `archive/tar` from the standard library.

### C. The build writes a tar to stdout

No code change. The build appends `tar c /out | base64`, and the caller parses
it back out of the log. It is capped at the 1 MiB log tail, mixed with the
build's own output, and every caller has to reinvent it. Listed only to say
why it isn't the answer.

## Storage

A `bytea` column on the job row, capped by `forge-worker -artifact-mb`
(default 16) and served by `GET /jobs/{id}/artifact` as `application/x-tar`.
There is one store, the API reads it the way it reads logs, and `-retain`
([deployment.md](deployment.md)) prunes artifacts with their jobs. When the
cap has to grow past what belongs in a row, an object store behind the same
endpoint is the upgrade.

A consumer that *extracts* the tar has to treat it as hostile input: absolute
paths, `..`, and links that point out of the extraction root. The supervisor
records links rather than following them, so the tar can contain them. That is
the consumer's boundary, and the endpoint's documentation has to say so.

## What B changes in the threat model

- Boundary 3 (no state outlives a job) holds: the layer still dies with the
  namespace.
- Boundary 14 (no host fd across execve) holds for the build. The new pipe
  belongs to the supervisor and is `O_CLOEXEC`.
- New row: *the build must not make the worker read outside `/out`, block it,
  or exceed the cap*. The mechanism is the walk inside the sandbox after
  `kill(-1)`, `lstat`-only, with a byte cap on both ends. The tests: a symlink
  to `/etc/passwd` is recorded, not followed. A unix socket in `/out` is skipped
  without blocking. A 2× over-cap `/out` is truncated and reported. A
  background process still writing `/out` after the build exits is dead
  before the walk.

## Decision needed

B, with the storage above, is the recommendation. It is roughly: the
supervisor (~150 lines), a `Spec.Artifacts io.Writer`, a queue column, and one
endpoint, each with the tests listed. The question for the owner is whether
builds should be able to hand back files at all. Until the answer is yes, the
sandbox stays as it is.
