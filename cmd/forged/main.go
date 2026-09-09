// Command forged runs a command inside a Forge sandbox. It is what
// forge-worker will fork for each build; for now it is driven by hand.
//
//	sudo forged -rootfs /var/lib/forge/alpine -memory-mb 64 -- /bin/sh -c 'ls /'
//
// Requires Linux and CAP_SYS_ADMIN. The command's own exit status is passed
// through; 125 means the sandbox could not be built, and the reason is on
// stderr. A build killed for exceeding its memory limit is reported as such
// rather than left to look like any other SIGKILL.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/parth-sharma-10/forge/internal/cgroup"
	"github.com/parth-sharma-10/forge/internal/sandbox"
)

func main() {
	// Before flag parsing: when Run re-executes this binary, argv belongs to
	// the sandbox, not to the flag set below.
	sandbox.MaybeChild()

	rootfs := flag.String("rootfs", "", "absolute path to the root filesystem the sandbox runs in")
	hostname := flag.String("hostname", "forge", "hostname inside the sandbox")
	memoryMB := flag.Int64("memory-mb", 0, "memory limit in MiB (0 for unlimited)")
	cpuCores := flag.Float64("cpu", 0, "CPU limit in cores, e.g. 0.5 (0 for unlimited)")
	maxPIDs := flag.Int("pids", 0, "maximum number of processes (0 for unlimited)")
	cgroupRoot := flag.String("cgroup-root", cgroup.DefaultRoot, "parent cgroup to create this job's cgroup under")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s -rootfs DIR [-hostname NAME] [limits] -- COMMAND [ARG...]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *rootfs == "" || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	// One signal tears the sandbox down: cancelling kills PID 1 of its PID
	// namespace, and the kernel reaps the rest with it. There is no drain to
	// offer — the build either finished or it did not.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := sandbox.Run(ctx, sandbox.Spec{
		Rootfs:     *rootfs,
		Hostname:   *hostname,
		Args:       flag.Args(),
		CgroupRoot: *cgroupRoot,
		Limits: cgroup.Limits{
			MemoryBytes: *memoryMB << 20,
			CPUCores:    *cpuCores,
			MaxPIDs:     *maxPIDs,
		},
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})

	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.Is(err, sandbox.ErrOOMKilled):
		// Named on the way out, because 137 alone does not say which of the
		// several things that send SIGKILL was responsible.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(137)
	case errors.As(err, &exitErr):
		os.Exit(exitErr.ExitCode())
	default:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
