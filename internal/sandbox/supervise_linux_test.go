//go:build linux

package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/parth-sharma-10/forge/internal/cgroup"
)

// collectRun runs spec with artifacts collected under limit, and returns the
// log, the archive's entries and Run's error. The archive must parse.
func collectRun(t *testing.T, spec Spec, limit int64) (string, map[string]entry, error) {
	t.Helper()
	var art bytes.Buffer
	if spec.Artifacts == nil {
		spec.Artifacts = &art
	}
	spec.ArtifactBytes = limit
	out, err := runSpec(t, spec)
	return out, readArchive(t, art.Bytes()), err
}

func collectSh(t *testing.T, script string, limit int64) (string, map[string]entry, error) {
	t.Helper()
	return collectRun(t, Spec{Rootfs: rootfs(t), Args: []string{"/bin/sh", "-c", script}}, limit)
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want an *exec.ExitError", err)
	}
	return exitErr.ExitCode()
}

func TestArtifactsAreHandedBack(t *testing.T) {
	out, got, err := collectSh(t, "mkdir -p /out/bin && echo report > /out/report.txt && "+
		"printf '#!/bin/sh\\n' > /out/bin/tool && chmod 755 /out/bin/tool", 1<<20)
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if got["report.txt"].body != "report\n" || got["bin/tool"].body != "#!/bin/sh\n" || got["bin/"].typ != tar.TypeDir {
		t.Fatalf("archive = %+v", got)
	}
}

// The status and the archive both come back from a build that failed: a
// failing test run is when its report is wanted most.
func TestASupervisedBuildKeepsItsExitStatus(t *testing.T) {
	out, got, err := collectSh(t, "mkdir /out && echo FAIL > /out/junit.xml; exit 7", 1<<20)
	if code := exitCode(t, err); code != 7 {
		t.Fatalf("exit = %d, want 7 (output %q)", code, out)
	}
	if got["junit.xml"].body != "FAIL\n" {
		t.Errorf("archive = %+v, want junit.xml from the failed build", got)
	}
}

// A build killed by a signal is reported killed — ExitCode -1, the signal
// named — though PID 1 cannot die of a signal it raises itself. It hands back
// no archive.
func TestASupervisedBuildKilledByASignalIsReportedKilled(t *testing.T) {
	out, got, err := collectSh(t, "mkdir /out && echo x > /out/a && kill -SEGV $$", 1<<20)
	if code := exitCode(t, err); code != -1 {
		t.Fatalf("exit = %d, want -1 for a signal (output %q)", code, out)
	}
	if !strings.Contains(err.Error(), "segmentation fault") {
		t.Errorf("err = %v, want the signal named", err)
	}
	if len(got) != 0 {
		t.Errorf("a killed build handed back %v", got)
	}
}

func TestASymlinkInOutIsRecordedNotFollowed(t *testing.T) {
	_, got, err := collectSh(t, "mkdir /out && ln -s /etc/passwd /out/passwd && ln -s /etc /out/etc", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if e := got["passwd"]; e.typ != tar.TypeSymlink || e.link != "/etc/passwd" {
		t.Errorf("passwd = %+v, want a symlink to /etc/passwd", e)
	}
	if len(got) != 2 {
		t.Errorf("archive = %+v, want the two links and nothing they point at", got)
	}
}

// bind is on the allowlist, so a build can leave a socket in /out. The walk
// skips it without blocking, and says so.
func TestASocketInOutIsSkippedWithoutBlocking(t *testing.T) {
	// Built here, not in the goroutine: a skip there would end only the
	// goroutine, and the test would go on to fail on a machine without root.
	spec := Spec{Rootfs: probeImage(t), Args: []string{"/forge-probe", probeArg, "out-socket"}}
	done := make(chan struct{})
	var out string
	var got map[string]entry
	var err error
	go func() {
		defer close(done)
		out, got, err = collectRun(t, spec, 1<<20)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("collection blocked on a socket in /out")
	}
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if len(got) != 1 || got["zz"].body != "after the socket" {
		t.Errorf("archive = %+v, want only zz", got)
	}
	if !strings.Contains(out, "skipped 1 entries") {
		t.Errorf("log = %q, want the skipped socket reported", out)
	}
}

func TestArtifactsOverTheCapAreTruncatedAndReported(t *testing.T) {
	const limit = 64 << 10
	out, got, err := collectSh(t, "mkdir /out && for f in a b c d; do head -c 32768 /dev/zero > /out/$f; done", limit)
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if len(got) == 0 || len(got) == 4 {
		t.Errorf("kept %d of 4 files at twice the cap", len(got))
	}
	if !strings.Contains(out, "over the 65536-byte cap") {
		t.Errorf("log = %q, want the truncation reported", out)
	}
}

// slowWriter holds the supervisor back: the pipe fills, and the walk takes as
// long as the writes do. The buffer is a field, not embedded, so io.Copy
// cannot go around Write through bytes.Buffer's ReadFrom.
type slowWriter struct{ buf bytes.Buffer }

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(50 * time.Millisecond)
	return w.buf.Write(p)
}

// Nothing of the build runs while /out is read. A background process writes
// /out/z/late 300ms after the build exits, and the walk is held back to well
// over a second before it reaches z/ — so the file is in the archive exactly
// when the background process outlived the build.
func TestABackgroundProcessIsDeadBeforeTheWalk(t *testing.T) {
	var art slowWriter
	script := "mkdir -p /out/z && for f in a b c d e f g h i j k l m n o p; do head -c 65536 /dev/zero > /out/$f; done; " +
		"(sleep 0.3; echo LATE > /out/z/late) & exit 0"
	start := time.Now()
	out, err := runSpec(t, Spec{Rootfs: rootfs(t), Args: []string{"/bin/sh", "-c", script},
		Artifacts: &art, ArtifactBytes: 4 << 20})
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if took := time.Since(start); took < time.Second {
		t.Fatalf("collection took %v; the test needs the walk slower than the background process", took)
	}
	got := readArchive(t, art.buf.Bytes())
	if _, ok := got["z/late"]; ok {
		t.Fatal("a background process was still writing /out during the walk")
	}
	if got["z/"].typ != tar.TypeDir {
		t.Fatalf("archive has %d entries and no z/: the walk never reached it", len(got))
	}
}

// Orphans are reparented to PID 1. A build that daemonises 60 short-lived
// processes under a 20-process limit fills it with zombies unless PID 1
// reaps them.
func TestTheSupervisorReapsOrphans(t *testing.T) {
	spec := Spec{Rootfs: rootfs(t), CgroupRoot: testCgroupRoot(t), Limits: cgroup.Limits{MaxPIDs: 20},
		Args: []string{"/bin/sh", "-c", "for i in $(seq 60); do (true &) || exit 3; done; echo forked all"}}
	out, _, err := collectRun(t, spec, 1<<20)
	if err != nil || !strings.Contains(out, "forked all") {
		t.Fatalf("run: %v (output %q): orphans were not reaped", err, out)
	}
}

// The supervisor holds what the build holds and no more, and the build
// cannot reach into it.
func TestTheSupervisorIsNoStrongerThanTheBuild(t *testing.T) {
	out, _, err := collectSh(t, "grep -E 'CapEff|VmRSS' /proc/1/status; "+
		"head -c1 /proc/1/mem >/dev/null 2>&1 && echo MEM-READABLE; "+
		"cat /proc/1/environ >/dev/null 2>&1 && echo ENVIRON-READABLE; true", 1<<20)
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "CapEff:\t00000000800004fb") {
		t.Errorf("supervisor capabilities: %q, want the keep list", out)
	}
	if strings.Contains(out, "READABLE") {
		t.Errorf("the build can read the supervisor's memory or environment: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "VmRSS") {
			t.Logf("supervisor resident while the build runs: %s", strings.Join(strings.Fields(line)[1:], " "))
		}
	}
}

// The artifact pipe is the supervisor's: the build gets stdio and nothing else.
func TestTheArtifactPipeDoesNotReachTheBuild(t *testing.T) {
	out, _, err := collectRun(t, Spec{Rootfs: probeImage(t), Args: []string{"/forge-probe", probeArg, "fd-leak"}}, 1<<20)
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "fd ") && !strings.HasPrefix(line, "fd 0 ") && !strings.HasPrefix(line, "fd 1 ") &&
			!strings.HasPrefix(line, "fd 2 ") && !strings.Contains(line, "/proc/") {
			t.Errorf("the build holds %q", line)
		}
	}
}

// Cancelling the context mid-collection kills the supervisor with the rest of
// the sandbox, and Run returns rather than waiting on the pipe.
func TestCancellingDuringCollectionReturns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var art slowWriter
	start := time.Now()
	err := Run(ctx, Spec{Rootfs: rootfs(t), Args: []string{"/bin/sh", "-c",
		"mkdir /out; for i in $(seq 40); do head -c 65536 /dev/zero > /out/$i; done"},
		Artifacts: &art, ArtifactBytes: 8 << 20})
	if code := exitCode(t, err); code != -1 {
		t.Fatalf("exit = %d, want -1 for a cancelled build", code)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("Run took %v after a 1s deadline", took)
	}
}

func TestArtifactsWithoutACapAreRefused(t *testing.T) {
	err := Run(context.Background(), Spec{Rootfs: rootfs(t), Args: []string{"true"}, Artifacts: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "ArtifactBytes") {
		t.Fatalf("err = %v, want the missing cap refused", err)
	}
}
