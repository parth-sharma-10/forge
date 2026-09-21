package worker

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/parth-sharma-10/forge/internal/metrics"
	"github.com/parth-sharma-10/forge/internal/sandbox"
)

// TestAJobIsKilledWhenItTimesOut: a build past its wall-clock limit has its
// context cancelled — which the sandbox turns into a kill — and is reported as
// a timeout, distinct from a lost lease or an operator's kill.
func TestAJobIsKilledWhenItTimesOut(t *testing.T) {
	store := newStore([]string{"/bin/sleep", "100"})
	var reg metrics.Registry
	killed := make(chan struct{})
	w := &Worker{Store: store, Lease: time.Second, Poll: 10 * time.Millisecond,
		Timeout: 30 * time.Millisecond, Metrics: &reg,
		Run: func(ctx context.Context, _ sandbox.Spec) error {
			<-ctx.Done() // the timeout is what ends this build
			close(killed)
			return exitWith(-1)
		}}
	serve(t, w)

	r := store.waitReport(t)
	select {
	case <-killed:
	default:
		t.Fatal("the build was not cancelled")
	}
	if !strings.Contains(r.Error, "timed out") {
		t.Fatalf("reported %+v, want a timeout error", r)
	}
	if got := timeoutCount(&reg); got != 1 {
		t.Fatalf("forge_jobs_done_total{outcome=timeout} = %d, want 1", got)
	}
}

// TestNoTimeoutMeansNoDeadline: with Timeout zero a long build is not killed
// for time — only the lease governs it.
func TestNoTimeoutMeansNoDeadline(t *testing.T) {
	store := newStore([]string{"/bin/true"})
	ran := make(chan struct{})
	w := &Worker{Store: store, Lease: time.Second, Poll: 10 * time.Millisecond,
		Run: func(ctx context.Context, _ sandbox.Spec) error {
			select {
			case <-ctx.Done():
				t.Error("build was cancelled though no timeout was set")
			case <-time.After(20 * time.Millisecond):
			}
			close(ran)
			return nil
		}}
	serve(t, w)
	store.waitReport(t)
	<-ran
}

func timeoutCount(reg *metrics.Registry) int64 {
	var b strings.Builder
	reg.WriteTo(&b)
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(line, `forge_jobs_done_total{outcome="timeout"}`) {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return n
				}
			}
		}
	}
	return 0
}
