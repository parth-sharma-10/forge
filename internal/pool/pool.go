// Package pool provides a bounded worker pool: N tasks, M workers, at most M
// tasks running at once. It exists because every part of Forge that fans work
// out (the worker's job loop, log spilling, cleanup sweeps) needs the same
// shutdown semantics: stop accepting, drain what was accepted, then exit.
package pool

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// ErrClosed is returned by Submit after Close has been called.
var ErrClosed = errors.New("pool: closed")

// Task is a unit of work. The context passed to it is the pool's root context,
// so a cancelled root cancels work already in flight.
type Task func(ctx context.Context) error

// Pool runs tasks on a fixed number of goroutines.
//
// Lifecycle: New -> Start -> Submit... -> Close -> Wait.
// Start must be called exactly once; Close and Wait are safe to call repeatedly.
type Pool struct {
	tasks   chan Task
	workers int
	log     *slog.Logger

	// closing is closed by Close before it waits for senders, so a Submit
	// parked on a full queue is released immediately instead of holding the
	// drain (and anything sequenced after it) until a worker frees a slot.
	closing chan struct{}

	// ctx is the root context captured by Start. It is stored rather than
	// threaded through Submit because Submit's blocking send must also abort
	// when the pool as a whole is cancelled, not just when the caller is.
	ctx context.Context

	mu      sync.RWMutex
	closed  bool
	senders sync.WaitGroup // in-flight Submit calls; Close waits for these
	workWG  sync.WaitGroup // running workers
	started bool
}

// New creates a pool with the given number of workers and queue capacity.
// A queue capacity of 0 makes Submit block until a worker is free.
func New(workers, queueSize int) *Pool {
	if workers < 1 {
		panic("pool: workers must be >= 1")
	}
	if queueSize < 0 {
		panic("pool: queueSize must be >= 0")
	}
	return &Pool{
		tasks:   make(chan Task, queueSize),
		closing: make(chan struct{}),
		workers: workers,
		log:     slog.Default(),
	}
}

// SetLogger overrides the logger used to report task errors.
func (p *Pool) SetLogger(l *slog.Logger) { p.log = l }

// Start launches the workers. Cancelling ctx stops workers and cancels tasks
// already running; it does not wait for them, use Wait for that.
func (p *Pool) Start(ctx context.Context) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		panic("pool: Start called twice")
	}
	p.started = true
	p.ctx = ctx
	p.mu.Unlock()

	for i := 0; i < p.workers; i++ {
		p.workWG.Add(1)
		go p.run(ctx, i)
	}
}

func (p *Pool) run(ctx context.Context, id int) {
	defer p.workWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case task, ok := <-p.tasks:
			if !ok {
				return // Close drained the queue
			}
			// Re-check: a cancelled root must not start new work even if the
			// queue still has tasks, since select picks randomly among ready cases.
			if ctx.Err() != nil {
				return
			}
			if err := task(ctx); err != nil {
				p.log.Warn("pool: task failed", "worker", id, "err", err)
			}
		}
	}
}

// Submit queues a task. It blocks while the queue is full and returns:
// ErrClosed if the pool is closed (including a Close that lands while this call
// is parked on a full queue), ctx.Err() if the caller's context ends, or the
// root context's error if the pool itself is cancelled.
//
// A nil return means the task was accepted and will run unless the pool's root
// context is cancelled first.
func (p *Pool) Submit(ctx context.Context, t Task) error {
	if t == nil {
		return errors.New("pool: nil task")
	}
	// Registering as a sender under the read lock is what makes Close safe:
	// Close flips closed under the write lock and then waits for every
	// registered sender to finish before closing the channel, so a send can
	// never race with the close.
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return ErrClosed
	}
	p.senders.Add(1)
	root := p.ctx
	p.mu.RUnlock()
	defer p.senders.Done()

	var rootDone <-chan struct{}
	if root != nil {
		rootDone = root.Done()
	}
	select {
	case p.tasks <- t:
		return nil
	case <-p.closing:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	case <-rootDone:
		return root.Err()
	}
}

// Close stops the pool accepting work and lets workers drain the queue.
// It does not wait for running tasks, use Wait. Close is idempotent and does
// not block on a full queue.
func (p *Pool) Close() {
	p.mu.Lock()
	already := p.closed
	p.closed = true
	p.mu.Unlock()
	if already {
		return
	}
	close(p.closing) // release any Submit parked on a full queue
	p.senders.Wait() // no Submit can be blocked on a send past this point
	close(p.tasks)
}

// Wait blocks until every worker has exited. Call Close first (or cancel the
// root context), otherwise workers never exit and Wait blocks forever.
func (p *Pool) Wait() { p.workWG.Wait() }
