// Command forge-worker will become Forge's job executor. Today it is a driver
// for internal/pool: it submits synthetic tasks and demonstrates the two
// shutdown modes the real worker will need.
//
//	first SIGINT/SIGTERM  -> drain: stop accepting, finish accepted work
//	second SIGINT/SIGTERM -> cancel: propagate cancellation into running tasks
package main

import (
	"context"
	"flag"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/parthrsharma/forge/internal/pool"
)

func main() {
	var (
		workers = flag.Int("workers", 4, "concurrent workers")
		queue   = flag.Int("queue", 8, "queued tasks before Submit blocks")
		tasks   = flag.Int("tasks", 20, "synthetic tasks to submit")
		dur     = flag.Duration("task-duration", 250*time.Millisecond, "how long each synthetic task runs")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := pool.New(*workers, *queue)
	p.Start(ctx)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Info("signal received, draining: no new work accepted")
		p.Close()
		<-sigs
		log.Warn("second signal, cancelling work in flight")
		cancel()
	}()

	for i := 0; i < *tasks; i++ {
		i := i
		err := p.Submit(ctx, func(taskCtx context.Context) error {
			log.Info("task started", "task", i)
			select {
			case <-time.After(jitter(*dur)):
				log.Info("task done", "task", i)
				return nil
			case <-taskCtx.Done():
				log.Info("task cancelled", "task", i)
				return taskCtx.Err()
			}
		})
		if err != nil {
			log.Info("stopped submitting", "submitted", i, "reason", err)
			break
		}
	}

	p.Close()
	p.Wait()
	log.Info("worker exited cleanly")
}

func jitter(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int63n(int64(d/2+1)))
}
