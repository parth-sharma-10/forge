// Command forge-worker claims jobs from the queue and runs them in the
// sandbox. It must run as root on Linux, like forged, and must call
// sandbox.MaybeChild before anything else, because the sandbox re-executes
// this binary as the child that builds the isolation.
//
//	first SIGINT/SIGTERM  -> drain: finish the jobs in hand, claim no more
//	second SIGINT/SIGTERM -> cancel: kill the jobs in hand
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/parth-sharma-10/forge/internal/cgroup"
	"github.com/parth-sharma-10/forge/internal/metrics"
	"github.com/parth-sharma-10/forge/internal/queue"
	"github.com/parth-sharma-10/forge/internal/sandbox"
	"github.com/parth-sharma-10/forge/internal/syscalls"
	"github.com/parth-sharma-10/forge/internal/worker"
)

func main() {
	sandbox.MaybeChild()

	hostname, _ := os.Hostname()
	var (
		dsn            = flag.String("dsn", os.Getenv("FORGE_DSN"), "PostgreSQL connection string (or FORGE_DSN)")
		rootfs         = flag.String("rootfs", "", "absolute path to the root filesystem jobs run in")
		workers        = flag.Int("workers", 4, "jobs run at once")
		name           = flag.String("name", hostname, "this worker's name, recorded on the jobs it claims")
		lease          = flag.Duration("lease", 30*time.Second, "how long a claim holds without a heartbeat")
		poll           = flag.Duration("poll", time.Second, "how long to wait after finding the queue empty")
		attempts       = flag.Int("max-attempts", 3, "claims a job may take before it is given up")
		timeout        = flag.Duration("timeout", time.Hour, "wall-clock limit on one build; 0 for no limit")
		memoryMB       = flag.Int64("memory-mb", 0, "memory limit per job in MiB (0 for unlimited)")
		cpuCores       = flag.Float64("cpu", 0, "CPU limit per job in cores (0 for unlimited)")
		maxPIDs        = flag.Int("pids", 0, "maximum processes per job (0 for unlimited)")
		diskMB         = flag.Int64("disk-mb", 0, "writable-layer (disk + inode) limit per job in MiB (0 for unlimited)")
		cgroupRoot     = flag.String("cgroup-root", cgroup.DefaultRoot, "parent cgroup for the jobs' cgroups")
		metricsAt      = flag.String("metrics", "", "address to serve /metrics on (empty for none)")
		retain         = flag.Duration("retain", 0, "delete finished jobs this long after they finish; 0 keeps them forever")
		syscallMetrics = flag.Bool("syscall-metrics", false, "count each job's syscalls via eBPF and export them (Linux, needs CAP_BPF; off is a no-op)")
	)
	flag.Parse()
	if *dsn == "" || *rootfs == "" {
		fmt.Fprintln(os.Stderr, "usage: forge-worker -dsn postgres://... -rootfs /path [flags]")
		flag.PrintDefaults()
		os.Exit(2)
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q, err := queue.Open(ctx, *dsn)
	if err != nil {
		log.Error("queue", "err", err)
		os.Exit(1)
	}
	defer q.Close()

	stop := make(chan struct{})
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Info("signal received, draining: finishing jobs in hand, claiming no more")
		close(stop)
		<-sigs
		log.Warn("second signal, killing jobs in hand")
		cancel()
	}()

	var reg metrics.Registry
	if *metricsAt != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", &reg)
		go func() {
			if err := http.ListenAndServe(*metricsAt, mux); err != nil {
				log.Error("metrics", "err", err)
			}
		}()
	}

	w := &worker.Worker{
		Store: q, Name: *name, Rootfs: *rootfs, CgroupRoot: *cgroupRoot,
		Limits:       cgroup.Limits{MemoryBytes: *memoryMB << 20, CPUCores: *cpuCores, MaxPIDs: *maxPIDs},
		ScratchBytes: *diskMB << 20,
		Lease:        *lease, Poll: *poll, MaxAttempts: *attempts, Timeout: *timeout, Log: log, Metrics: &reg,
	}

	if *syscallMetrics {
		obs, err := syscalls.Load(&reg, log)
		if err != nil {
			// Observability is optional: a worker that cannot load the program
			// runs jobs identically, it just exports no syscall histogram.
			log.Warn("syscall metrics off", "err", err)
		} else {
			defer obs.Close()
			w.Observe = obs.Job()
			log.Info("syscall metrics on")
		}
	}
	if *retain > 0 {
		go prune(ctx, q, *retain, log)
	}
	log.Info("worker started", "name", *name, "workers", *workers, "rootfs", *rootfs)
	w.Serve(ctx, stop, *workers)
	log.Info("worker exited cleanly")
}

// prune deletes finished jobs older than retain. It runs every tenth of
// retain, clamped between once a minute and once an hour. Every worker runs
// it; a DELETE of rows another worker already deleted removes nothing, so they
// need no coordination.
func prune(ctx context.Context, q *queue.Queue, retain time.Duration, log *slog.Logger) {
	t := time.NewTicker(min(max(retain/10, time.Minute), time.Hour))
	defer t.Stop()
	for {
		n, err := q.Prune(ctx, retain)
		if err != nil {
			log.Warn("prune failed", "err", err)
		} else if n > 0 {
			log.Info("pruned finished jobs", "count", n, "older_than", retain)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
