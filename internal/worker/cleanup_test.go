package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/parth-sharma-10/forge/internal/metrics"
	"github.com/parth-sharma-10/forge/internal/sandbox"
)

// TestACleanupFailureIsCountedNotReported: a build that succeeded but left its
// cgroup behind is reported as the success it was, and the leak is counted.
func TestACleanupFailureIsCountedNotReported(t *testing.T) {
	store := newStore([]string{"ok"}, []string{"fail"})
	var reg metrics.Registry
	w := &Worker{Store: store, Lease: time.Second, Poll: 10 * time.Millisecond, Metrics: &reg,
		Run: func(_ context.Context, spec sandbox.Spec) error {
			var build error
			if spec.Args[0] == "fail" {
				build = exitWith(4)
			}
			return &sandbox.CleanupError{Build: build, Cleanup: errors.New("cgroup: remove job-1: still busy")}
		}}
	serve(t, w)

	if r := store.waitReport(t); r.ExitCode != 0 || r.Error != "" {
		t.Fatalf("a successful build with a leaked cgroup was reported as %+v", r)
	}
	if r := store.waitReport(t); r.ExitCode != 4 || r.Error != "" {
		t.Fatalf("a failed build with a leaked cgroup was reported as %+v", r)
	}
	var b strings.Builder
	reg.WriteTo(&b)
	for _, line := range []string{
		"forge_sandbox_cleanup_failures_total 2\n",
		"forge_jobs_done_total{outcome=\"ok\"} 1\n",
		"forge_jobs_done_total{outcome=\"failed\"} 1\n",
	} {
		if !strings.Contains(b.String(), line) {
			t.Errorf("metrics missing %q:\n%s", line, b.String())
		}
	}
}
