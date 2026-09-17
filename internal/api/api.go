// Package api is Forge's HTTP front door: submit a job, read its state,
// follow its log. Standard library only.
//
//	POST /jobs            {"args": ["/bin/sh", "-c", "make"]}  -> 201 {"id": 7}
//	GET  /jobs/{id}                                            -> 200 job JSON, 404
//	GET  /jobs/{id}/logs                                       -> 200 text/plain, streamed until the job is done
//	GET  /metrics                                              -> the queue's depth by state, Prometheus text format
//
// The API trusts its network: there is no authentication. It is meant to
// sit behind whatever does that, and the README says so.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/parth-sharma-10/forge/internal/metrics"
	"github.com/parth-sharma-10/forge/internal/queue"
)

// Store is what the handlers need from the queue.
type Store interface {
	Submit(ctx context.Context, args []string) (int64, error)
	Get(ctx context.Context, id int64) (queue.Status, error)
	Counts(ctx context.Context) (map[string]int64, error)
}

// followEvery is how often a followed log re-reads the row. The worker
// flushes every two seconds at most, so reading faster than this buys
// nothing but load.
const followEvery = 500 * time.Millisecond

// Handler serves the API over store.
func Handler(store Store) http.Handler {
	return (&server{store: store, followEvery: followEvery}).routes()
}

type server struct {
	store       Store
	followEvery time.Duration
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", s.submit)
	mux.HandleFunc("GET /jobs/{id}", s.get)
	mux.HandleFunc("GET /jobs/{id}/logs", s.logs)
	var reg metrics.Registry
	reg.Sampled("forge_queue_jobs", "Jobs in the queue, by state.", func() ([]metrics.Sample, error) {
		counts, err := s.store.Counts(context.Background())
		if err != nil {
			return nil, err
		}
		var out []metrics.Sample
		for _, state := range []string{"queued", "running", "done"} {
			out = append(out, metrics.Sample{Labels: map[string]string{"state": state}, Value: counts[state]})
		}
		return out, nil
	})
	mux.Handle("GET /metrics", &reg)
	return mux
}

// Job is the wire form of a job's state.
type Job struct {
	ID      int64  `json:"id"`
	State   string `json:"state"`
	Attempt int    `json:"attempt"`
	Worker  string `json:"worker,omitempty"`
	// ExitCode is present once the job is done: the command's status, or
	// -1 if it was killed or never ran.
	ExitCode  *int   `json:"exit_code,omitempty"`
	OOMKilled bool   `json:"oom_killed"`
	Error     string `json:"error,omitempty"`
	// LogBytes is how much output the job has produced so far — more than
	// /logs returns if the worker's tail limit was passed.
	LogBytes int64 `json:"log_bytes"`
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Args []string `json:"args"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "body must be JSON with an args array: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.Args) == 0 {
		http.Error(w, "args must name a command", http.StatusBadRequest)
		return
	}
	id, err := s.store.Submit(r.Context(), body.Args)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/jobs/%d", id))
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	id, st, ok := s.lookup(w, r)
	if !ok {
		return
	}
	j := Job{ID: id, State: st.State, Attempt: st.Attempt, Worker: st.Worker,
		OOMKilled: st.Result.OOMKilled, Error: st.Result.Error, LogBytes: st.Result.Log.Total}
	if st.State == "done" {
		code := st.Result.ExitCode
		j.ExitCode = &code
	}
	writeJSON(w, http.StatusOK, j)
}

// logs writes the job's output and, while the job runs, keeps writing as
// more arrives, until it is done. Each read of the row is compared with the
// last by the total byte count, and only the new bytes are sent; if more
// arrived than the row's tail holds, the reader is told how many it missed
// rather than handed a splice that reads as continuous.
func (s *server) logs(w http.ResponseWriter, r *http.Request) {
	id, st, ok := s.lookup(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)

	var sent int64
	for {
		log := st.Result.Log
		// A total below what was already sent means another worker took
		// the job over and its output started again from nothing.
		if log.Total < sent {
			fmt.Fprintf(w, "\n[forge: job restarted, attempt %d]\n", st.Attempt)
			sent = 0
		}
		if missed := log.Total - sent - int64(len(log.Tail)); missed > 0 {
			fmt.Fprintf(w, "\n[forge: %d bytes of output not kept]\n", missed)
			sent += missed
		}
		w.Write(log.Tail[len(log.Tail)-int(log.Total-sent):])
		sent = log.Total
		if flusher != nil {
			flusher.Flush()
		}
		if st.State == "done" {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(s.followEvery):
		}
		var err error
		if st, err = s.store.Get(r.Context(), id); err != nil {
			return
		}
	}
}

func (s *server) lookup(w http.ResponseWriter, r *http.Request) (int64, queue.Status, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "job id must be a number", http.StatusBadRequest)
		return 0, queue.Status{}, false
	}
	st, err := s.store.Get(r.Context(), id)
	if errors.Is(err, queue.ErrNotFound) {
		http.Error(w, "no such job", http.StatusNotFound)
		return 0, queue.Status{}, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return 0, queue.Status{}, false
	}
	return id, st, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
