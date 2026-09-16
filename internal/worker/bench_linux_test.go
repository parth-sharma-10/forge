//go:build linux

package worker

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/parth-sharma-10/forge/internal/cgroup"
)

// BenchmarkThroughput is jobs per second through the whole path: b.N jobs
// of /bin/true submitted to a real queue, four worker loops claiming them
// and running them in the real sandbox, until the last is reported. ns/op
// is the wall time per job with four in flight. Root, an image and a
// database; `make bench`.
func BenchmarkThroughput(b *testing.B) {
	rootfs := os.Getenv("FORGE_TEST_ROOTFS")
	if rootfs == "" || os.Geteuid() != 0 {
		b.Skip("needs root and FORGE_TEST_ROOTFS")
	}
	// openQueue empties the table: a queue carrying the dead rows of every
	// earlier run measures PostgreSQL's bloat, not Forge.
	q, ctx := openQueue(b), context.Background()
	cgroupRoot, err := os.MkdirTemp("/sys/fs/cgroup", "forge-bench-")
	if err != nil {
		b.Fatal(err)
	}
	defer os.Remove(cgroupRoot)

	var last int64
	for i := 0; i < b.N; i++ {
		if last, err = q.Submit(ctx, []string{"/bin/true"}); err != nil {
			b.Fatal(err)
		}
	}
	w := &Worker{Store: q, Name: "bench", Rootfs: rootfs, CgroupRoot: cgroupRoot,
		Limits: cgroup.Limits{MemoryBytes: 64 << 20, MaxPIDs: 32}, Lease: 10 * time.Second, Poll: 10 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	serveCtx, cancel := context.WithCancel(ctx)
	stop, done := make(chan struct{}), make(chan struct{})
	b.ResetTimer()
	go func() {
		defer close(done)
		w.Serve(serveCtx, stop, 4)
	}()
	for {
		s, err := q.Get(ctx, last)
		if err != nil {
			b.Fatal(err)
		}
		if s.State == "done" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	b.StopTimer()
	close(stop)
	cancel()
	<-done
}
