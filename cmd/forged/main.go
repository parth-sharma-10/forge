// Command forged runs a command inside a Forge sandbox. It is what
// forge-worker will fork for each build; for now it is driven by hand.
//
//	sudo forged -rootfs /var/lib/forge/alpine -- /bin/sh -c 'id; hostname; ls /'
//
// Requires Linux and CAP_SYS_ADMIN. The command's own exit status is passed
// through; 125 means the sandbox could not be built, and the reason is on
// stderr.
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

	"github.com/parth-sharma-10/forge/internal/sandbox"
)

func main() {
	// Before flag parsing: when Run re-executes this binary, argv belongs to
	// the sandbox, not to the flag set below.
	sandbox.MaybeChild()

	rootfs := flag.String("rootfs", "", "absolute path to the root filesystem the sandbox runs in")
	hostname := flag.String("hostname", "forge", "hostname inside the sandbox")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s -rootfs DIR [-hostname NAME] -- COMMAND [ARG...]\n\n", os.Args[0])
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
		Rootfs:   *rootfs,
		Hostname: *hostname,
		Args:     flag.Args(),
		Stdin:    os.Stdin,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
	})

	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		os.Exit(exitErr.ExitCode())
	default:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
