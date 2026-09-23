// Package sandbox runs an untrusted command inside a Linux sandbox that Forge
// builds from kernel primitives directly — namespaces, private mount
// propagation and pivot_root — rather than by shelling out to a container
// runtime.
//
// The isolation cannot be applied to the calling process: unsharing the mount
// namespace and pivot_root'ing away the host filesystem would take the rest of
// the program with it. So Run re-executes the running binary
// (/proc/self/exe) with the namespace flags set, and that second process
// applies the isolation to itself before exec'ing the build command over
// itself. Every binary that calls Run must therefore call MaybeChild first, so
// the re-executed copy takes the child path instead of running main again.
//
//	func main() {
//		sandbox.MaybeChild() // returns immediately unless this is the re-exec
//		...
//		err := sandbox.Run(ctx, sandbox.Spec{Rootfs: "/var/lib/forge/alpine", Args: argv})
//	}
//
// Only the Linux build implements this. Elsewhere Run returns ErrUnsupported.
package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/parth-sharma-10/forge/internal/cgroup"
)

// ErrUnsupported is returned by Run on platforms without Linux namespaces.
var ErrUnsupported = errors.New("sandbox: requires Linux")

// ErrOOMKilled wraps the error from Run when the kernel killed the build for
// exceeding Spec.Limits.MemoryBytes.
//
// An OOM kill reaches userspace as SIGKILL — exit status 137 through a shell —
// which is the same thing an operator's `kill -9` and a lease timeout look
// like. Only the cgroup knows the difference, so Run asks it. The returned
// error still unwraps to *exec.ExitError for the status itself.
var ErrOOMKilled = errors.New("sandbox: killed by the memory limit")

// CleanupError is returned by Run when the build ran but its cgroup could not
// be removed afterwards. Build is what Run would otherwise have returned — nil
// for a build that succeeded — and it stays reachable through errors.Is and
// errors.As, so a caller reading the exit status still finds it. The cleanup
// failure is the operator's problem, not the build's: a cgroup left behind is
// a kernel object leaked once per job, and the leak ends in ENOSPC on the
// hierarchy, so it must be seen, but it says nothing about what the build did.
type CleanupError struct {
	Build   error
	Cleanup error
}

func (e *CleanupError) Error() string {
	if e.Build == nil {
		return e.Cleanup.Error()
	}
	return fmt.Sprintf("%v (and %v)", e.Build, e.Cleanup)
}

func (e *CleanupError) Unwrap() []error { return []error{e.Build, e.Cleanup} }

// ChildArg is the argv[1] sentinel that marks a process as the re-executed
// child stage. It is exported because a binary that dispatches on its own
// subcommands has to know which one not to claim.
const ChildArg = "forge-sandbox-child"

// SetupExitCode is the exit status of the child when the sandbox fails to
// build — as distinct from a non-zero status of the build command itself,
// which is passed through untouched.
const SetupExitCode = 125

const (
	envRootfs   = "FORGE_SANDBOX_ROOTFS"
	envHostname = "FORGE_SANDBOX_HOSTNAME"
	envScratch  = "FORGE_SANDBOX_SCRATCH_BYTES"
	// envPreflight makes the child build the whole sandbox and exit 0 in
	// place of exec'ing anything. See Preflight.
	envPreflight = "FORGE_SANDBOX_PREFLIGHT"

	defaultHostname = "forge"
)

// Spec describes one sandboxed execution.
//
// ponytail: the spec crosses into the child as environment variables, which
// only stays workable while every field is a scalar. The moment it carries a
// mount list or a resource limit set, marshal it as JSON over an extra fd.
type Spec struct {
	// Rootfs is an absolute path to the directory that becomes / inside the
	// sandbox. It is bind-mounted onto itself (pivot_root refuses a new root
	// that is not a mount point) and left otherwise untouched.
	Rootfs string

	// Hostname is set inside the UTS namespace. Empty means "forge".
	Hostname string

	// Args is the command and its arguments. Args[0] is resolved against the
	// sandbox's PATH, inside the new root, not the host's.
	Args []string

	// Limits are the cgroups v2 resource caps. The zero value is no limits at
	// all, which is what a build gets until a caller says otherwise.
	Limits cgroup.Limits

	// CgroupRoot is the parent cgroup that this job's cgroup is created under.
	// Empty means cgroup.DefaultRoot; under systemd delegation, pass the
	// delegated path.
	CgroupRoot string

	// ScratchBytes caps the writable overlay layer — everything a build
	// creates or changes — in bytes, and its inode count with it. Zero means
	// no cap of its own, in which case the layer is bounded only by the
	// cgroup's memory limit (the tmpfs pages are charged to it). A cap here
	// makes a build filling the disk fail with ENOSPC rather than an OOM kill,
	// and bounds inode exhaustion, which a memory limit alone does not.
	ScratchBytes int64

	// Observe, if non-nil, is called with the job's cgroup id once the cgroup
	// exists and before the build starts; the func it returns is called after
	// the build has exited and before the cgroup is removed. It is the seam the
	// syscall observer enrols and drains a job through. It must never fail a
	// build: an error obtaining the id skips observation for that job, and a nil
	// Observe disables it entirely.
	Observe func(cgroupID uint64) (done func())

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	preflight bool
}

func (s *Spec) validate() error {
	if len(s.Args) == 0 {
		return errors.New("sandbox: spec has no command")
	}
	if !filepath.IsAbs(s.Rootfs) {
		return fmt.Errorf("sandbox: rootfs %q must be an absolute path", s.Rootfs)
	}
	if strings.ContainsAny(s.Rootfs, ",:") {
		return fmt.Errorf("sandbox: rootfs %q: ',' and ':' are separators in overlay mount options", s.Rootfs)
	}
	fi, err := os.Stat(s.Rootfs)
	if err != nil {
		return fmt.Errorf("sandbox: rootfs: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("sandbox: rootfs %q is not a directory", s.Rootfs)
	}
	if s.Hostname == "" {
		s.Hostname = defaultHostname
	}
	return s.Limits.Validate()
}
