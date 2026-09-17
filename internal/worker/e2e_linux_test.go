//go:build linux

package worker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/parth-sharma-10/forge/internal/cgroup"
	"github.com/parth-sharma-10/forge/internal/queue"
	"github.com/parth-sharma-10/forge/internal/sandbox"
)

// TestMain gives this test binary the sandbox's dual identity, since the
// end-to-end test below runs the real thing.
func TestMain(m *testing.M) {
	sandbox.MaybeChild()
	os.Exit(m.Run())
}

// TestEndToEnd is Phase 2's exit criterion: a job submitted to a real queue
// is claimed by the real loop, runs in the real sandbox, and its exit
// status, its log and the OOM distinction come back through the queue. It
// needs root, an image and a database — the CI test-sandbox step has all
// three, and refuses to skip there.
func TestEndToEnd(t *testing.T) {
	rootfs, dsn := os.Getenv("FORGE_TEST_ROOTFS"), os.Getenv("FORGE_TEST_DSN")
	if rootfs == "" || os.Geteuid() != 0 {
		t.Skip("needs root and FORGE_TEST_ROOTFS: run `make test-sandbox`")
	}
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("FORGE_TEST_DSN is not set: the end-to-end test must run in CI")
		}
		t.Skip("FORGE_TEST_DSN not set")
	}
	ctx := context.Background()
	q, err := queue.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)

	cgroupRoot, err := os.MkdirTemp("/sys/fs/cgroup", "forge-worker-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(cgroupRoot) })

	plain, err := q.Submit(ctx, []string{"/bin/sh", "-c", "echo hello; echo warn >&2; hostname; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	hog, err := q.Submit(ctx, []string{"/bin/sh", "-c", "x=aaaaaaaaaaaaaaaa; while :; do x=$x$x; done"})
	if err != nil {
		t.Fatal(err)
	}

	w := &Worker{
		Store: q, Name: "e2e", Rootfs: rootfs, CgroupRoot: cgroupRoot,
		Limits: cgroup.Limits{MemoryBytes: 32 << 20, MaxPIDs: 64},
		Lease:  2 * time.Second, Poll: 50 * time.Millisecond,
	}
	serveCtx, cancel := context.WithCancel(ctx)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		w.Serve(serveCtx, stop, 2)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	wait := func(id int64) queue.Status {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			s, err := q.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if s.State == "done" {
				return s
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("job %d did not finish within a minute", id)
		return queue.Status{}
	}

	s := wait(plain)
	if s.Worker != "e2e" || s.Attempt != 1 {
		t.Errorf("plain job: %+v, want attempt 1 by e2e", s)
	}
	if s.Result.ExitCode != 3 || s.Result.OOMKilled || s.Result.Error != "" {
		t.Errorf("plain job result: %+v, want exit 3", s.Result)
	}
	if got := string(s.Result.Log); got != "hello\nwarn\nforge\n" {
		t.Errorf("plain job log = %q: the sandbox's output did not reach the queue", got)
	}

	// The shell itself is what the kernel kills, so there is no exit status
	// — 137 is a shell's convention for a *child* killed by signal.
	s = wait(hog)
	if !s.Result.OOMKilled || s.Result.ExitCode != -1 || s.Result.Error != "" {
		t.Errorf("memory hog result: %+v, want OOMKilled with exit -1 and no error", s.Result)
	}

	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not drain")
	}
	if _, err := q.Claim(ctx, "after", time.Minute); !errors.Is(err, queue.ErrEmpty) {
		t.Fatalf("a job was left unclaimed or unreported: %v", err)
	}
}
