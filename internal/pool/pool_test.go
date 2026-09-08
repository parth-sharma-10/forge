package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quiet(p *Pool) *Pool {
	p.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return p
}

func TestRunsEveryTaskExactlyOnce(t *testing.T) {
	const n = 1000
	p := quiet(New(4, 16))
	p.Start(context.Background())

	var runs [n]atomic.Int32
	for i := 0; i < n; i++ {
		i := i
		if err := p.Submit(context.Background(), func(context.Context) error {
			runs[i].Add(1)
			return nil
		}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	p.Close()
	p.Wait()

	for i := 0; i < n; i++ {
		if got := runs[i].Load(); got != 1 {
			t.Fatalf("task %d ran %d times, want 1", i, got)
		}
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	const workers = 3
	p := quiet(New(workers, 0))
	p.Start(context.Background())

	var live, peak atomic.Int64
	for i := 0; i < 200; i++ {
		if err := p.Submit(context.Background(), func(context.Context) error {
			cur := live.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			live.Add(-1)
			return nil
		}); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	p.Close()
	p.Wait()

	if peak.Load() > workers {
		t.Fatalf("peak concurrency %d exceeds %d workers", peak.Load(), workers)
	}
	if peak.Load() < 2 {
		t.Fatalf("peak concurrency %d, expected the workers to actually overlap", peak.Load())
	}
}

func TestSubmitAfterCloseFails(t *testing.T) {
	p := quiet(New(1, 1))
	p.Start(context.Background())
	p.Close()

	err := p.Submit(context.Background(), func(context.Context) error { return nil })
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit after Close = %v, want ErrClosed", err)
	}
	p.Wait()
}

func TestCloseDrainsQueuedWork(t *testing.T) {
	// One worker held busy, so the remaining tasks sit in the queue when Close
	// is called. Graceful shutdown must still run them.
	p := quiet(New(1, 10))
	release := make(chan struct{})
	p.Start(context.Background())

	var done atomic.Int64
	if err := p.Submit(context.Background(), func(context.Context) error {
		<-release
		done.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := p.Submit(context.Background(), func(context.Context) error {
			done.Add(1)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	p.Close()
	close(release)
	p.Wait()

	if got := done.Load(); got != 6 {
		t.Fatalf("ran %d tasks after Close, want all 6 drained", got)
	}
}

func TestCancelPropagatesToRunningTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := quiet(New(2, 0))
	p.Start(ctx)

	started := make(chan struct{})
	var saw atomic.Value
	if err := p.Submit(context.Background(), func(taskCtx context.Context) error {
		close(started)
		<-taskCtx.Done()
		saw.Store(taskCtx.Err())
		return taskCtx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()

	waitOrFail(t, p, 2*time.Second)
	if got, _ := saw.Load().(error); !errors.Is(got, context.Canceled) {
		t.Fatalf("task saw %v, want context.Canceled", got)
	}
}

func TestSubmitUnblocksWhenPoolCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := quiet(New(1, 0)) // unbuffered: the second Submit must block
	p.Start(ctx)

	busy := make(chan struct{})
	if err := p.Submit(context.Background(), func(taskCtx context.Context) error {
		close(busy)
		<-taskCtx.Done()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-busy

	errc := make(chan error, 1)
	go func() {
		errc <- p.Submit(context.Background(), func(context.Context) error { return nil })
	}()
	time.Sleep(20 * time.Millisecond) // let the Submit park on the send
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked Submit returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Submit never returned after the pool was cancelled")
	}
	waitOrFail(t, p, 2*time.Second)
}

func TestSubmitRespectsCallerContext(t *testing.T) {
	p := quiet(New(1, 0))
	p.Start(context.Background())

	busy := make(chan struct{})
	release := make(chan struct{})
	if err := p.Submit(context.Background(), func(context.Context) error {
		close(busy)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-busy

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := p.Submit(ctx, func(context.Context) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Submit = %v, want DeadlineExceeded", err)
	}

	close(release)
	p.Close()
	p.Wait()
}

// Submit racing Close is the case that panics with "send on closed channel" if
// the sender bookkeeping is wrong. Run under -race.
func TestConcurrentSubmitAndClose(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		p := quiet(New(4, 8))
		p.Start(context.Background())

		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := p.Submit(context.Background(), func(context.Context) error { return nil })
				if err != nil && !errors.Is(err, ErrClosed) {
					t.Errorf("Submit = %v, want nil or ErrClosed", err)
				}
			}()
		}
		go p.Close()
		wg.Wait()
		p.Close() // idempotent
		p.Wait()
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		p := quiet(New(8, 4))
		ctx, cancel := context.WithCancel(context.Background())
		p.Start(ctx)
		for j := 0; j < 100; j++ {
			_ = p.Submit(ctx, func(context.Context) error { return nil })
		}
		p.Close()
		p.Wait()
		cancel()
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > base {
		t.Fatalf("goroutines: %d before, %d after", base, got)
	}
}

func TestTaskErrorsDoNotKillWorkers(t *testing.T) {
	p := quiet(New(2, 4))
	p.Start(context.Background())

	var ran atomic.Int64
	for i := 0; i < 20; i++ {
		if err := p.Submit(context.Background(), func(context.Context) error {
			ran.Add(1)
			return errors.New("boom")
		}); err != nil {
			t.Fatal(err)
		}
	}
	p.Close()
	p.Wait()

	if got := ran.Load(); got != 20 {
		t.Fatalf("ran %d tasks, want 20 despite every task erroring", got)
	}
}

func waitOrFail(t *testing.T, p *Pool, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("pool did not shut down in time")
	}
}

// Regression: Close must not wait for a Submit parked on a full queue. It used
// to, which delayed everything sequenced after the drain, including the hard
// stop in cmd/forge-worker.
func TestCloseReleasesParkedSubmit(t *testing.T) {
	p := quiet(New(1, 0))
	p.Start(context.Background())

	busy := make(chan struct{})
	release := make(chan struct{})
	if err := p.Submit(context.Background(), func(context.Context) error {
		close(busy)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-busy

	parked := make(chan error, 1)
	go func() {
		parked <- p.Submit(context.Background(), func(context.Context) error { return nil })
	}()
	time.Sleep(20 * time.Millisecond) // let it park on the send

	closed := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		p.Close()
		closed <- time.Since(start)
	}()

	select {
	case err := <-parked:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("parked Submit = %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("parked Submit was not released by Close")
	}
	select {
	case d := <-closed:
		if d > 500*time.Millisecond {
			t.Fatalf("Close took %v; it must not wait for the running task", d)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind the running task")
	}

	close(release)
	p.Wait()
}
