//go:build linux

package sandbox

import (
	"fmt"
	"strings"
	"testing"
)

// TestCgroupNamespaceHidesTheHostCgroupPath: with CLONE_NEWCGROUP the build's
// /proc/self/cgroup is rooted at its own cgroup, so it cannot read the host
// path it actually sits at — no "forge/job-XXXX" leaks through.
func TestCgroupNamespaceHidesTheHostCgroupPath(t *testing.T) {
	out, err := sh(t, "cat /proc/self/cgroup")
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	// v2 lines are "0::<path>"; namespaced, the path is "/".
	if !strings.HasPrefix(out, "0::") {
		t.Fatalf("/proc/self/cgroup = %q, not a cgroup v2 line", out)
	}
	if strings.Contains(out, "job-") || strings.Contains(out, "/forge") {
		t.Fatalf("/proc/self/cgroup = %q leaks the host cgroup path; the cgroup namespace is not hiding it", out)
	}
}

// TestTheDiskLimitStopsAHostileFill: a build that tries to fill the disk past
// its scratch limit gets ENOSPC, not the host's free space. A build inside the
// limit is untouched.
func TestTheDiskLimitStopsAHostileFill(t *testing.T) {
	const cap = 8 << 20 // 8 MiB

	over, err := runSpec(t, Spec{
		Rootfs: rootfs(t), Hostname: "forge-test", ScratchBytes: cap,
		Args: []string{"/bin/sh", "-c", "dd if=/dev/zero of=/big bs=1M count=64 2>&1 || echo HITCAP"},
	})
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, over)
	}
	if !strings.Contains(over, "No space left") && !strings.Contains(over, "HITCAP") {
		t.Fatalf("a 64 MiB write under an 8 MiB cap was not stopped; output %q", over)
	}

	under, err := runSpec(t, Spec{
		Rootfs: rootfs(t), Hostname: "forge-test", ScratchBytes: cap,
		Args: []string{"/bin/sh", "-c", "dd if=/dev/zero of=/small bs=1M count=4 2>/dev/null && echo OK"},
	})
	if err != nil {
		t.Fatalf("a 4 MiB write under an 8 MiB cap failed: %v (output %q)", err, under)
	}
	if under != "OK" {
		t.Fatalf("a write inside the cap did not succeed; output %q", under)
	}
}

// TestTheInodeLimitStopsFileExhaustion: the scratch cap bounds inodes too, so
// a build creating many tiny files runs out of inodes long before it runs out
// of bytes — an exhaustion a byte limit alone would not stop.
func TestTheInodeLimitStopsFileExhaustion(t *testing.T) {
	const cap = 8 << 20 // 8 MiB -> ~2100 inodes

	out, err := runSpec(t, Spec{
		Rootfs: rootfs(t), Hostname: "forge-test", ScratchBytes: cap,
		Args: []string{"/bin/sh", "-c", "i=0; while [ $i -lt 8000 ]; do : > /f$i || break; i=$((i+1)); done; echo made $i"},
	})
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	var made int
	if _, scanErr := fmt.Sscanf(out, "made %d", &made); scanErr != nil {
		t.Fatalf("unexpected output %q", out)
	}
	if made >= 8000 || made == 0 {
		t.Fatalf("created %d files; the inode cap did not bite (empty files, so bytes were not the limit)", made)
	}
}
