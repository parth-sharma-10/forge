//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// artifactFD is where the supervisor finds its end of the artifact pipe: the
// first of cmd.ExtraFiles.
const artifactFD = 3

// supervise is the child's last step when the job collects artifacts. In
// place of exec'ing the build over itself it stays as PID 1, a small init of
// Forge's own, and:
//
//  1. forks the build and execs it with stdio only — the artifact pipe is
//     close-on-exec, so no host descriptor crosses into the build;
//  2. reaps every process that ends up its child, orphans included, until
//     the build itself exits;
//  3. kills everything left in the PID namespace and reaps it, so nothing of
//     the build is running while /out is read — no symlink swapped in after
//     a check, no file growing during the copy;
//  4. writes the build's fate to the pipe, then the archive of /out;
//  5. exits with the build's status.
//
// Writing a signal number first is how a build killed by a signal stays
// reported as killed. PID 1 cannot die of a signal it sends itself, so it
// says which one and waits; the parent kills it with SIGKILL and wraps the
// real signal into the error. A killed build hands back no artifacts.
//
// It runs under everything the build does — the namespaces, the dropped
// bounding set, the seccomp filter — and, by the time it is called, holds
// only the capabilities a build keeps and cannot be ptraced by the build
// (limitSupervisor). Its input is the tree a hostile build left behind, and
// writeArchive reads it as one.
func supervise(path string, argv, env []string, limit int64) error {
	syscall.CloseOnExec(artifactFD)
	pipe := os.NewFile(artifactFD, "artifacts")

	pid, err := syscall.ForkExec(path, argv, &syscall.ProcAttr{Env: env, Files: []uintptr{0, 1, 2}})
	if err != nil {
		return fmt.Errorf("sandbox: start %s: %w", path, err)
	}
	status, err := reapUntil(pid)
	if err != nil {
		return err
	}
	killAll()

	if status.Signaled() {
		pipe.Write([]byte{byte(status.Signal())})
		for {
			time.Sleep(time.Hour) // until the parent's SIGKILL
		}
	}
	if _, err := pipe.Write([]byte{0}); err != nil {
		os.Exit(status.ExitStatus())
	}
	if err := writeArchive(ArtifactDir, pipe, limit, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "forge: %v\n", err)
	}
	pipe.Close()
	os.Exit(status.ExitStatus())
	return nil
}

// reapUntil waits on every child until pid exits, and returns its status.
// As PID 1 the supervisor inherits every orphan in the namespace; reaping
// them as they come keeps a build that daemonises from filling the pids
// limit with zombies. An error means the build's status is unknown, which
// must not read as a success: it fails the job as a setup failure.
func reapUntil(pid int) (syscall.WaitStatus, error) {
	for {
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(-1, &ws, 0, nil)
		switch {
		case err == syscall.EINTR:
			continue
		case err != nil:
			return 0, fmt.Errorf("sandbox: wait for the build: %w", err)
		case got == pid:
			return ws, nil
		}
	}
}

// killAll SIGKILLs every other process in the PID namespace and reaps them
// all. kill(-1) from PID 1 reaches everything but PID 1; it is repeated after
// each reap, so a process forked while the first one was in flight is
// caught by the next. ESRCH means nothing was left to signal, ECHILD that
// nothing is left to reap: every survivor would have an ancestor that is
// this process's child.
func killAll() {
	for {
		if err := syscall.Kill(-1, syscall.SIGKILL); err == syscall.ESRCH {
			return
		}
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(-1, &ws, 0, nil); err == syscall.ECHILD {
			return
		}
	}
}

const (
	prSetDumpable = 4          // PR_SET_DUMPABLE
	capV3         = 0x20080522 // _LINUX_CAPABILITY_VERSION_3
)

// limitSupervisor makes the supervisor hold no more than the build it
// starts. It runs after the bounding set is dropped and before the seccomp
// filter, which denies capset.
//
// The bounding set limits what an exec confers, not what the process already
// holds: without this the supervisor would keep every capability of its user
// namespace — CAP_SYS_ADMIN among them — while it walks the build's tree.
// The capability sets are cut to keptCapabilities — on this thread, which is
// the one child() is pinned to, so the one that forks the build and walks
// the tree; capset, like the bounding set, is per thread. That still lets it read
// every file (CAP_DAC_OVERRIDE) and kill every process (CAP_KILL) the build
// can leave behind.
//
// Non-dumpable means a process without CAP_SYS_PTRACE in the namespace —
// the build — cannot ptrace it or open its /proc/1/mem, whatever the host's
// Yama setting. execve resets the flag, so the build is dumpable as before.
func limitSupervisor() error {
	var mask [2]uint32
	for _, c := range keptCapabilities {
		mask[c/32] |= 1 << (c % 32)
	}
	hdr := struct {
		version uint32
		pid     int32
	}{version: capV3}
	data := [2]struct{ effective, permitted, inheritable uint32 }{
		{mask[0], mask[0], 0}, {mask[1], mask[1], 0},
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("sandbox: capset: %w", errno)
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return fmt.Errorf("sandbox: prctl(PR_SET_DUMPABLE): %w", errno)
	}
	return nil
}
