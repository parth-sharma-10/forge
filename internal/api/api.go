// Package api is Forge's HTTP front door: submit a job, read its state,
// follow its log. Standard library only.
//
//	POST /jobs            {"args": ["/bin/sh", "-c", "make"]}  -> 201 {"id": 7}
//	GET  /jobs?state=running&limit=50&before=7                 -> 200 {"jobs": [...], "next": 3}, newest first
//	GET  /jobs/{id}                                            -> 200 job JSON, 404
//	GET  /jobs/{id}/logs                                       -> 200 text/plain, streamed until the job is done
//	POST /jobs/{id}/cancel                                     -> 200 done, 202 running (killed within a heartbeat), 404, 409 already done
//	GET  /metrics                                              -> the queue's depth by state, Prometheus text format
//
// Authentication is a static bearer token, applied to every route including
// /metrics: a request must carry `Authorization: Bearer <token>`. It is the
// simplest thing that is a real boundary — one shared secret, compared in
// constant time — for the trusted/internal deployment Forge targets, not a
// user/role system. A deployment that terminates auth in a proxy in front can
// run the API with no token (Handler called with ""), which turns the check
// off; cmd/forge-api makes that an explicit, loud choice rather than a default.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/parth-sharma-10/forge/internal/metrics"
	"github.com/parth-sharma-10/forge/internal/queue"
)

// Store is what the handlers need from the queue.
type Store interface {
	Submit(ctx context.Context, args []string) (int64, error)
	Get(ctx context.Context, id int64) (queue.Status, error)
	Counts(ctx context.Context) (map[string]int64, error)
	Cancel(ctx context.Context, id int64) (string, error)
	List(ctx context.Context, state string, before int64, limit int) ([]queue.Summary, error)
}

// Listing bounds: a page is defaultPage jobs unless the caller asks for up to
// maxPage. No log is listed, so a page is small either way.
const (
	defaultPage = 50
	maxPage     = 500
)

// followEvery is how often a followed log re-reads the row. The worker
// flushes every two seconds at most, so reading faster than this buys
// nothing but load.
const followEvery = 500 * time.Millisecond

// Handler serves the API over store, requiring the bearer token on every
// request. An empty token turns authentication off — a choice cmd/forge-api
// forces the operator to make explicitly.
func Handler(store Store, token string) http.Handler {
	return (&server{store: store, followEvery: followEvery, token: token}).routes()
}

type server struct {
	store       Store
	followEvery time.Duration
	token       string
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", s.submit)
	mux.HandleFunc("GET /jobs", s.list)
	mux.HandleFunc("GET /jobs/{id}", s.get)
	mux.HandleFunc("GET /jobs/{id}/logs", s.logs)
	mux.HandleFunc("POST /jobs/{id}/cancel", s.cancel)
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
	return s.authenticated(mux)
}

// authenticated rejects any request without the bearer token. With no token
// configured it passes everything through — the proxy-terminates-auth mode.
//
// The comparison is constant-time so a caller cannot recover the token one
// byte at a time from response timing; the length is folded in so comparing
// against a wrong-length input is not itself a timing signal.
func (s *server) authenticated(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeEq(int32(len(got)), int32(len(want))) != 1 ||
			subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="forge"`)
			http.Error(w, "unauthorized: valid bearer token required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Job is the wire form of a job's state.
type Job struct {
	ID int64 `json:"id"`
	// Args is the command; set in a listing, where it is what tells the jobs
	// apart.
	Args    []string `json:"args,omitempty"`
	State   string   `json:"state"`
	Attempt int      `json:"attempt"`
	Worker  string   `json:"worker,omitempty"`
	// ExitCode is present once the job is done: the command's status, or
	// -1 if it was killed or never ran.
	ExitCode  *int   `json:"exit_code,omitempty"`
	OOMKilled bool   `json:"oom_killed"`
	Error     string `json:"error,omitempty"`
	// CancelRequested is set once a cancel has been asked for. A running job
	// stays running until its worker's next heartbeat kills it, and one whose
	// build exited on its own in that interval keeps its own exit status, so
	// this says what was asked, not how the job ended; Error says that.
	CancelRequested bool `json:"cancel_requested,omitempty"`
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
	if len(body.Args) == 0 || body.Args[0] == "" {
		http.Error(w, "args must name a command", http.StatusBadRequest)
		return
	}
	// JSON can carry a NUL as \u0000, and neither PostgreSQL text nor an
	// execve argument can hold one: refused here as the client's error rather
	// than reaching the database as a 500 carrying its message.
	for _, a := range body.Args {
		if strings.IndexByte(a, 0) >= 0 {
			http.Error(w, "args must not contain NUL bytes", http.StatusBadRequest)
			return
		}
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
	writeJSON(w, http.StatusOK, wireJob(id, st))
}

func wireJob(id int64, st queue.Status) Job {
	j := Job{ID: id, State: st.State, Attempt: st.Attempt, Worker: st.Worker, CancelRequested: st.Cancelled,
		OOMKilled: st.Result.OOMKilled, Error: st.Result.Error, LogBytes: st.Result.Log.Total}
	if st.State == "done" {
		code := st.Result.ExitCode
		j.ExitCode = &code
	}
	return j
}

// list pages through the jobs newest first. next, when present, is the
// before= that fetches the following page.
func (s *server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	switch state {
	case "", "queued", "running", "done":
	default:
		http.Error(w, "state must be queued, running or done", http.StatusBadRequest)
		return
	}
	limit, before := defaultPage, int64(0)
	var err error
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > maxPage {
			http.Error(w, fmt.Sprintf("limit must be 1 to %d", maxPage), http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("before"); v != "" {
		if before, err = strconv.ParseInt(v, 10, 64); err != nil || before < 1 {
			http.Error(w, "before must be a job id", http.StatusBadRequest)
			return
		}
	}
	jobs, err := s.store.List(r.Context(), state, before, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	page := struct {
		Jobs []Job `json:"jobs"`
		Next int64 `json:"next,omitempty"`
	}{Jobs: make([]Job, 0, len(jobs))}
	for _, j := range jobs {
		wj := wireJob(j.ID, j.Status)
		wj.Args = j.Args
		page.Jobs = append(page.Jobs, wj)
	}
	// A full page may have more behind it; a short one is the end.
	if len(jobs) == limit {
		page.Next = jobs[len(jobs)-1].ID
	}
	writeJSON(w, http.StatusOK, page)
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

// cancel cancels a job. A queued job is finished on the spot (200); a running
// one is marked and killed by its worker at the next heartbeat (202, since the
// build may still be running when the response arrives). A finished job's
// result stands (409).
func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "job id must be a number", http.StatusBadRequest)
		return
	}
	state, err := s.store.Cancel(r.Context(), id)
	switch {
	case errors.Is(err, queue.ErrNotFound):
		http.Error(w, "no such job", http.StatusNotFound)
	case errors.Is(err, queue.ErrDone):
		http.Error(w, "job already done", http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	case state == "done":
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "state": state})
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "state": state})
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
