// Command forge-api serves the HTTP API over the queue: POST /jobs,
// GET /jobs/{id}, GET /jobs/{id}/logs. It needs no privileges and does not
// touch the sandbox; the worker does.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/parth-sharma-10/forge/internal/api"
	"github.com/parth-sharma-10/forge/internal/queue"
)

func main() {
	var (
		dsn      = flag.String("dsn", os.Getenv("FORGE_DSN"), "PostgreSQL connection string (or FORGE_DSN)")
		listen   = flag.String("listen", "127.0.0.1:8080", "address to serve on")
		token    = flag.String("auth-token", os.Getenv("FORGE_API_TOKEN"), "bearer token required on every request (or FORGE_API_TOKEN)")
		insecure = flag.Bool("insecure-no-auth", false, "serve with no authentication (only behind a proxy that authenticates)")
	)
	flag.Parse()
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "usage: forge-api -dsn postgres://... -auth-token TOKEN [-listen addr]")
		flag.PrintDefaults()
		os.Exit(2)
	}
	// Default deny: an API that runs untrusted-command submission must not come
	// up unauthenticated by accident. Either a token, or a deliberate opt-out.
	if *token == "" && !*insecure {
		fmt.Fprintln(os.Stderr, "refusing to serve without authentication: set -auth-token / FORGE_API_TOKEN, or pass -insecure-no-auth if a proxy authenticates in front")
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if *token == "" {
		log.Warn("authentication is OFF (-insecure-no-auth): every caller can submit and read jobs")
	}

	q, err := queue.Open(context.Background(), *dsn)
	if err != nil {
		log.Error("queue", "err", err)
		os.Exit(1)
	}
	defer q.Close()

	srv := &http.Server{Addr: *listen, Handler: api.Handler(q, *token), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
		<-sigs
		log.Info("signal received, shutting down")
		// Long enough for a followed log to notice; a follower still
		// attached after that is cut off.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	log.Info("api listening", "addr", *listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
	log.Info("api exited cleanly")
}
