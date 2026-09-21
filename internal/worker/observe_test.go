package worker

import (
	"context"
	"testing"
	"time"

	"github.com/parth-sharma-10/forge/internal/sandbox"
)

// TestObserveIsForwardedToTheSandbox: the worker passes its Observe seam into
// every job's spec, which is how the syscall observer enrols a job's cgroup.
func TestObserveIsForwardedToTheSandbox(t *testing.T) {
	store := newStore([]string{"/bin/true"})
	var got sandbox.Spec
	w := &Worker{Store: store, Lease: time.Second, Poll: 10 * time.Millisecond,
		Observe: func(uint64) func() { return nil },
		Run:     func(_ context.Context, spec sandbox.Spec) error { got = spec; return nil }}
	serve(t, w)
	store.waitReport(t)
	if got.Observe == nil {
		t.Fatal("the worker did not forward Observe to the sandbox spec")
	}
}

// TestJobsRunUnobservedWhenObserveIsNil: observation is optional. A worker
// with no observer runs and reports jobs unchanged and passes no seam down.
func TestJobsRunUnobservedWhenObserveIsNil(t *testing.T) {
	store := newStore([]string{"/bin/true"})
	var got sandbox.Spec
	w := &Worker{Store: store, Lease: time.Second, Poll: 10 * time.Millisecond,
		Run: func(_ context.Context, spec sandbox.Spec) error { got = spec; return nil }}
	serve(t, w)
	if r := store.waitReport(t); r.ExitCode != 0 {
		t.Fatalf("reported %+v", r)
	}
	if got.Observe != nil {
		t.Fatal("Observe should be nil when the worker has no observer")
	}
}
