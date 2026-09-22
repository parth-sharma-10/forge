#!/usr/bin/env python3
"""Mutation harness for the sandbox: the evidence that its tests assert anything.

Each mutation breaks one property a test is supposed to pin. The suite is
built with the mutation applied, run as root inside the VM, and the mutation is
CAUGHT if the run fails and MISSED if the suite still passes. A missed
mutation is a test that proves nothing, and the harness exits non-zero on it.

    make mutate                       # every sandbox mutation, in the VM
    make mutate-queue                 # the queue's, on the host, against FORGE_TEST_DSN
    hack/mutate.py identity-map       # just the ones named

Three traps this harness guards against, each of which has cost a session
(docs/failures.md, 2026-09-09 and 2026-09-11):
  * a mutation that does not compile is a HARNESS ERROR, never a result --
    otherwise the previous mutation's binary runs again and everything after
    the first failure looks caught;
  * every binary path is unique per batch and per mutation, so a stale one
    cannot be executed;
  * a mutation can hang instead of fail (one deadlocks Go's fork path before
    any assertion runs), so the test binary gets a deadline, and a hang is
    read from stderr -- the panic goes there while `--- FAIL` goes to stdout.
"""

import os
import pathlib
import platform
import shutil
import subprocess
import sys
import uuid

FORGE = pathlib.Path(__file__).resolve().parent.parent
SANDBOX = "internal/sandbox/sandbox_linux.go"
SECCOMP = "internal/sandbox/seccomp_linux.go"
SECCOMP_ARM64 = "internal/sandbox/seccomp_linux_arm64.go"
QUEUE = "internal/queue/queue.go"
BINDIR = pathlib.Path.home() / ".cache/forge/mut"
ROOTFS = os.environ.get("FORGE_TEST_ROOTFS", "/var/tmp/forge-rootfs")
VM = os.environ.get("VM", "colima")
GOARCH = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64"}[platform.machine()]
BATCH = uuid.uuid4().hex[:8]

# (name, file, old, new). `old` must occur exactly once in `file`.
MUTATIONS = [
    # --- the private /dev (Week 2) ---
    (
        "no-dev-at-all",
        SANDBOX,
        "\tif err := mountDev(rootfs); err != nil {\n\t\treturn err\n\t}\n",
        "",
    ),
    # Retired 2026-09-16 with the per-job overlay: the mutation dropped the
    # tmpfs under /dev and was caught by nodes leaking into the shared image,
    # which the overlay now prevents on its own. Equivalent, so not scored.
    (
        "no-nodes",
        SANDBOX,
        '\tfor _, n := range devNodes {\n\t\tsrc := "/dev/" + n.name\n',
        '\tfor _, n := range devNodes[:0] {\n\t\tsrc := "/dev/" + n.name\n',
    ),
    (
        "no-symlinks",
        SANDBOX,
        "\tfor _, l := range devLinks {\n",
        "\tfor _, l := range devLinks[:0] {\n",
    ),
    ("full-has-nulls-minor", SANDBOX, '{"full", 1, 7},', '{"full", 1, 3},'),
    (
        "mkdev-packs-side-by-side",
        SANDBOX,
        "return int((minor & 0xff) | (major << 8) | ((minor &^ 0xff) << 12))",
        "return int((major << 20) | minor)",
    ),
    # The device set widening by one node is what "someone exposed more of
    # /dev" looks like: /dev/mem is the whole of physical memory, and adding
    # it is a one-line change nobody notices without a test that pins the set.
    (
        "extra-node-dev-mem",
        SANDBOX,
        '\t{"null", 1, 3},',
        '\t{"null", 1, 3},\n\t{"mem", 1, 1},',
    ),
    # --- the seccomp filter (Week 4) ---
    # The filter is loaded and looks right, but decides nothing.
    (
        "unlisted-syscalls-allowed",
        SECCOMP,
        "const denyAction = retErrno | uint32(syscall.ENOSYS)",
        "const denyAction = retAllow",
    ),
    # The errno a denial returns is the whole compatibility argument for the
    # allowlist. Only one test should care.
    (
        "denial-returns-eperm",
        SECCOMP,
        "const denyAction = retErrno | uint32(syscall.ENOSYS)",
        "const denyAction = retErrno | uint32(syscall.EPERM)",
    ),
    # No architecture check at all: the numbers get matched against whatever
    # table the caller happens to be using.
    (
        "no-arch-check",
        SECCOMP,
        "\t\tsyscall.SockFilter{Code: bpfLoadWordAbs, K: offsetArch},\n"
        "\t\tsyscall.SockFilter{Code: bpfJumpEqual, Jt: 1, K: auditArch},\n"
        "\t\tsyscall.SockFilter{Code: bpfReturn, K: retKillProcess},\n",
        "",
    ),
    (
        "other-abi-allowed-not-killed",
        SECCOMP,
        "syscall.SockFilter{Code: bpfReturn, K: retKillProcess},",
        "syscall.SockFilter{Code: bpfReturn, K: retAllow},",
    ),
    # The audit token names a different architecture, so every syscall is
    # issued under the "wrong" ABI.
    (
        "wrong-audit-arch",
        SECCOMP_ARM64,
        "const auditArch = 0xc00000b7",
        "const auditArch = 0xc000003e",
    ),
    # Match and mismatch swapped: the comparison chain still exists and still
    # has the right numbers in it.
    (
        "jump-branch-inverted",
        SECCOMP,
        "syscall.SockFilter{Code: bpfJumpEqual, Jf: 1, K: uint32(nr)},",
        "syscall.SockFilter{Code: bpfJumpEqual, Jt: 1, K: uint32(nr)},",
    ),
    # Everything is built correctly and never installed.
    (
        "filter-never-installed",
        SANDBOX,
        "\tif err := applySeccomp(); err != nil {\n\t\treturn err\n\t}\n",
        "",
    ),
    (
        "no-new-privs-not-set",
        SECCOMP,
        "\tif _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {\n"
        '\t\treturn fmt.Errorf("sandbox: set no_new_privs: %w", errno)\n\t}\n',
        "",
    ),
    # Not here: a mutation that drops SECCOMP_FILTER_FLAG_TSYNC. Whether the
    # thread that installs the filter is the one that reaches execve is up to
    # Go's scheduler, so the suite passes under it and no test can pin it.
    # The allowlist widening by one entry is what "someone allowed a bit more"
    # looks like: a one-line diff nobody notices without a test that pins what
    # is denied.
    (
        "mknod-added-to-allowlist",
        SECCOMP,
        "\t\tsyscall.SYS_UMASK,",
        "\t\tsyscall.SYS_UMASK,\n\t\tsyscall.SYS_MKNODAT,",
    ),
    # And the opposite failure, which no test of what is denied can see: an
    # allowlist one entry too tight breaks real builds and nothing else.
    ("allowlist-one-entry-too-tight", SECCOMP, "\t\tsyscall.SYS_GETDENTS64,\n", ""),
    # --- the user namespace (Week 5) ---
    # The map is written and the process never moves into it: the child keeps
    # the host root it was cloned from, and every id inside is the real thing.
    # This is the mutation that makes the whole change decoration.
    (
        "credential-not-applied",
        SANDBOX,
        "\t\tCredential:                 &syscall.Credential{Uid: 0, Gid: 0},\n",
        "",
    ),
    # A user namespace that maps root to root. Every namespace is still
    # created, /proc still shows PID 1, and nothing is isolated by it.
    ("identity-map", SANDBOX, "mappedBase  = 524288", "mappedBase  = 0"),
    # Only the id the build runs as is mapped, which is the tempting smaller
    # version of this change and breaks every package manager.
    ("single-id-map", SANDBOX, "mappedCount = 65536", "mappedCount = 1"),
    # The one line of the change with no effect a build can see.
    (
        "setgroups-enabled",
        SANDBOX,
        "GidMappingsEnableSetgroups: false,",
        "GidMappingsEnableSetgroups: true,",
    ),
    # No user namespace at all, with the maps left in place. Go's fork path
    # deadlocks on this one before any assertion runs; it is caught by the
    # deadline, not by a test, and is scored that way.
    (
        "no-user-namespace",
        SANDBOX,
        "const namespaces = syscall.CLONE_NEWUSER | // user:    sandbox root is an unprivileged host uid\n\tsyscall.CLONE_NEWNS |",
        "const namespaces = syscall.CLONE_NEWNS |",
    ),
    # --- the per-job overlay (Phase 1) ---
    # No overlay: the image bind-mounted onto itself, as before Phase 1. Every
    # write a build makes lands in the image and the next build inherits it.
    (
        "no-overlay-image-bound-directly",
        SANDBOX,
        "\tif err := mountOverlay(rootfs); err != nil {\n\t\treturn err\n\t}\n",
        '\tif err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {\n'
        '\t\treturn fmt.Errorf("sandbox: bind rootfs %s: %w", rootfs, err)\n\t}\n',
    ),
    # The lower layer is resolved from cwd, which is only the image because the
    # child moved there first. Without that step "." is the host's root.
    (
        "lower-is-cwd-not-image",
        SANDBOX,
        '\tif err := syscall.Chdir(rootfs); err != nil {\n'
        '\t\treturn fmt.Errorf("sandbox: chdir %s: %w", rootfs, err)\n\t}\n'
        '\tif err := syscall.Mount("tmpfs", rootfs, "tmpfs", 0, "mode=0755"); err != nil {',
        '\tif err := syscall.Mount("tmpfs", rootfs, "tmpfs", 0, "mode=0755"); err != nil {',
    ),
    # Not scored: dropping ",userxattr". On Ubuntu's 6.8 the mount sets its
    # trusted.* xattrs from inside the user namespace without complaint, so no
    # test can see the option; docs/decisions.md 2026-09-16 has the account.
    # --- clone flags (Phase 1) ---
    # The flag test compares against nothing, so it never jumps to the
    # refusal: clone is back to being allowed by number alone.
    (
        "clone-flags-unchecked",
        SECCOMP,
        "syscall.SockFilter{Code: bpfJumpSet, Jt: 1, K: cloneNamespaceFlags},",
        "syscall.SockFilter{Code: bpfJumpSet, Jt: 1, K: 0},",
    ),
    # One namespace missing from the mask — the one that matters most.
    (
        "clone-newuser-not-in-mask",
        SECCOMP,
        "\t\tsyscall.CLONE_NEWUSER | syscall.CLONE_NEWPID |",
        "\t\tsyscall.CLONE_NEWPID |",
    ),
    # --- capability bounding set (Phase 1) ---
    (
        "bounding-set-not-dropped",
        SANDBOX,
        "\tif err := dropBoundingSet(); err != nil {\n\t\treturn err\n\t}\n",
        "",
    ),
    # Everything dropped but the one capability the exercise is for.
    (
        "sys-admin-kept",
        SANDBOX,
        "\t10, // CAP_NET_BIND_SERVICE\n",
        "\t10, // CAP_NET_BIND_SERVICE\n\t21,\n",
    ),
]

# The queue's mutations run on the host against FORGE_TEST_DSN: they need
# PostgreSQL and no privileges. Same shape, different target.
QUEUE_MUTATIONS = [
    # Report matches the row without the fencing token, so a worker that
    # lost its lease can overwrite the result of the one that took the job.
    # The clause replacing it is typed: `$2 = $2` on its own is a parameter
    # PostgreSQL cannot infer, and the statement errors instead of landing.
    (
        "report-without-fence",
        QUEUE,
        "\t\t    finished_at = now(), lease_until = NULL\n"
        "\t\tWHERE id = $1 AND fence = $2 AND state = 'running'`,",
        "\t\t    finished_at = now(), lease_until = NULL\n"
        "\t\tWHERE id = $1 AND $2::bigint >= 0 AND state = 'running'`,",
    ),
    # Heartbeat likewise: a stale worker keeps extending a lease it lost.
    (
        "heartbeat-without-fence",
        QUEUE,
        "\t\tWHERE id = $1 AND fence = $2 AND state = 'running'\n"
        "\t\tRETURNING cancel_requested`,",
        "\t\tWHERE id = $1 AND $2::bigint >= 0 AND state = 'running'\n"
        "\t\tRETURNING cancel_requested`,",
    ),
    # The heartbeat never passes a cancel on: a cancelled build runs to the end.
    (
        "cancel-not-heard",
        QUEUE,
        "\t\tRETURNING cancel_requested`,",
        "\t\tRETURNING false`,",
    ),
    # Every heartbeat rewrites the log, changed or not: 1.15 MB of WAL a beat.
    (
        "heartbeat-rewrites-log",
        QUEUE,
        "\t\t    log = CASE WHEN log_bytes = $5 THEN log ELSE COALESCE($4, ''::bytea) END,\n",
        "\t\t    log = COALESCE($4, ''::bytea),\n",
    ),
    # A reclaim keeps the dead attempt's log, so a new attempt whose output
    # is the same length never replaces it.
    (
        "claim-keeps-old-log",
        QUEUE,
        "\t\t    worker = $1, lease_until = now() + $2, started_at = now(),\n"
        "\t\t    log = '', log_bytes = 0\n",
        "\t\t    worker = $1, lease_until = now() + $2, started_at = now()\n",
    ),
    # Cancel reaches a finished job and rewrites the result it already has.
    (
        "cancel-overwrites-done",
        QUEUE,
        "\t\tWHERE id = $1 AND state <> 'done'\n",
        "\t\tWHERE id = $1\n",
    ),
    # An expired lease is never reclaimed: a dead worker's job is stuck.
    (
        "no-reclaim",
        QUEUE,
        "\t\t\tWHERE state = 'queued' OR (state = 'running' AND lease_until < now())\n",
        "\t\t\tWHERE state = 'queued'\n",
    ),
    # The schema applied without the advisory lock: two processes starting
    # together on a fresh database race on CREATE TABLE IF NOT EXISTS.
    (
        "no-schema-lock",
        QUEUE,
        "`SELECT pg_advisory_xact_lock($1)`",
        "`SELECT $1::bigint`",
    ),
]


def build(binary):
    env = dict(os.environ, GOOS="linux", GOARCH=GOARCH)
    return subprocess.run(
        ["go", "test", "-c", "-o", str(binary), "./internal/sandbox"],
        cwd=FORGE, env=env, capture_output=True, text=True,
    )


def build_host(binary):
    return subprocess.run(
        ["go", "test", "-c", "-o", str(binary), "./internal/queue"],
        cwd=FORGE, capture_output=True, text=True,
    )


def run_on_host(binary):
    # The queue's own tests, and the worker's multi-worker tests, are what
    # notice these; the worker package is run from source since the
    # mutation is in the queue package it imports.
    if not os.environ.get("FORGE_TEST_DSN"):
        sys.exit("FORGE_TEST_DSN is not set: the queue mutations need a PostgreSQL")
    first = subprocess.run(
        [str(binary), "-test.count=1", "-test.timeout=120s"],
        cwd=FORGE, capture_output=True, text=True,
    )
    if first.returncode != 0:
        return first
    return subprocess.run(
        ["go", "test", "-count=1", "-timeout=120s", "./internal/worker"],
        cwd=FORGE, capture_output=True, text=True,
    )


def run_in_vm(binary):
    script = (
        f"cp {binary} /tmp/forge-mut-{binary.stem}.test && "
        f"sudo FORGE_TEST_ROOTFS={ROOTFS} /tmp/forge-mut-{binary.stem}.test "
        f"-test.count=1 -test.timeout=180s"
    )
    return subprocess.run([VM, "ssh", "--", "sh", "-c", script], capture_output=True, text=True)


def failing_tests(output):
    return sorted(
        {line.split()[2].rstrip(":") for line in output.splitlines() if line.startswith("--- FAIL:")}
    )


def main():
    args = sys.argv[1:]
    on_host = "--queue" in args
    only = set(a for a in args if a != "--queue")
    mutations = QUEUE_MUTATIONS if on_host else MUTATIONS
    originals = {rel: (FORGE / rel).read_text() for _, rel, _, _ in mutations}
    BINDIR.mkdir(parents=True, exist_ok=True)
    results, harness_errors = [], []
    try:
        for name, rel, old, new in mutations:
            if only and name not in only:
                continue
            path, source = FORGE / rel, originals[rel]
            if source.count(old) != 1:
                harness_errors.append(f"{name}: pattern matches {source.count(old)} times in {rel}, want 1")
                continue
            path.write_text(source.replace(old, new, 1))
            try:
                binary = BINDIR / f"{BATCH}-{name}.test"
                built = build_host(binary) if on_host else build(binary)
                if built.returncode != 0 or not binary.exists():
                    harness_errors.append(f"{name}: does not compile\n{built.stderr.strip()[:400]}")
                    continue
                got = run_on_host(binary) if on_host else run_in_vm(binary)
            finally:
                path.write_text(source)
            caught = got.returncode != 0
            hung = "panic: test timed out" in got.stdout + got.stderr
            results.append((name, caught))
            detail = failing_tests(got.stdout) or ("hung, no assertion reached" if hung else "suite passed")
            print(f"{'CAUGHT' if caught else 'MISSED'} {name}: {detail}", flush=True)
    finally:
        for rel, text in originals.items():
            (FORGE / rel).write_text(text)
        shutil.rmtree(BINDIR, ignore_errors=True)

    print("\n--- summary ---")
    print(f"{sum(1 for _, c in results if c)}/{len(results)} mutations caught")
    for e in harness_errors:
        print(f"HARNESS ERROR: {e}")
    missed = [n for n, c in results if not c]
    if missed:
        print(f"MISSED: {missed}")
    return 1 if (missed or harness_errors) else 0


if __name__ == "__main__":
    sys.exit(main())
