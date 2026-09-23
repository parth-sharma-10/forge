package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/parth-sharma-10/forge/internal/queue"
)

// fakeStore is the queue in memory, driven by the test as if it were the
// worker: the handler's own logic — the wire format, the follow loop, the
// truncation arithmetic — is what these tests are about, and the queue's
// contract is proven in its own package against PostgreSQL.
type fakeStore struct {
	mu   sync.Mutex
	jobs map[int64]queue.Status
}

func newStore() *fakeStore { return &fakeStore{jobs: map[int64]queue.Status{}} }

func (s *fakeStore) Submit(_ context.Context, args []string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := int64(len(s.jobs) + 1)
	s.jobs[id] = queue.Status{State: "queued"}
	return id, nil
}

func (s *fakeStore) Get(_ context.Context, id int64) (queue.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.jobs[id]
	if !ok {
		return queue.Status{}, queue.ErrNotFound
	}
	return st, nil
}

func (s *fakeStore) Counts(context.Context) (map[string]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int64{}
	for _, st := range s.jobs {
		counts[st.State]++
	}
	return counts, nil
}

// set is the worker's side: what a heartbeat or a report would have written.
func (s *fakeStore) set(id int64, st queue.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[id] = st
}

func newServer(t *testing.T) (*httptest.Server, *fakeStore) {
	t.Helper()
	store := newStore()
	// Poll fast: the tests drive the store by hand and should not wait on
	// the production interval.
	srv := httptest.NewServer((&server{store: store, followEvery: 10 * time.Millisecond}).routes())
	t.Cleanup(srv.Close)
	return srv, store
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestSubmitAndGet(t *testing.T) {
	srv, store := newServer(t)
	resp := post(t, srv.URL+"/jobs", `{"args": ["/bin/sh", "-c", "make"]}`)
	var created struct{ ID int64 }
	json.NewDecoder(resp.Body).Decode(&created)
	if resp.StatusCode != http.StatusCreated || created.ID != 1 || resp.Header.Get("Location") != "/jobs/1" {
		t.Fatalf("submit: %d, id %d, Location %q", resp.StatusCode, created.ID, resp.Header.Get("Location"))
	}

	resp, body := get(t, srv.URL+"/jobs/1")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"state":"queued"`) || strings.Contains(body, "exit_code") {
		t.Fatalf("get while queued: %d %s", resp.StatusCode, body)
	}

	store.set(1, queue.Status{State: "done", Attempt: 1, Worker: "w1",
		Result: queue.Result{ExitCode: 3, Log: queue.Log{Tail: []byte("hi\n"), Total: 3}}})
	_, body = get(t, srv.URL+"/jobs/1")
	var j Job
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		t.Fatal(err)
	}
	if j.State != "done" || j.ExitCode == nil || *j.ExitCode != 3 || j.Worker != "w1" || j.LogBytes != 3 {
		t.Fatalf("get when done: %s", body)
	}
}

func TestBadRequests(t *testing.T) {
	srv, _ := newServer(t)
	for _, body := range []string{`{}`, `{"args": []}`, `not json`, ``, `{"args": [""]}`, `{"args": ["sh", "a\u0000b"]}`} {
		if resp := post(t, srv.URL+"/jobs", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST /jobs with %q: %d, want 400", body, resp.StatusCode)
		}
	}
	if resp, _ := get(t, srv.URL+"/jobs/abc"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /jobs/abc: %d, want 400", resp.StatusCode)
	}
}

func TestUnknownJobIs404(t *testing.T) {
	srv, _ := newServer(t)
	for _, path := range []string{"/jobs/42", "/jobs/42/logs"} {
		if resp, _ := get(t, srv.URL+path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestLogsOfADoneJobReturnAtOnce(t *testing.T) {
	srv, store := newServer(t)
	post(t, srv.URL+"/jobs", `{"args": ["true"]}`)
	store.set(1, queue.Status{State: "done", Result: queue.Result{Log: queue.Log{Tail: []byte("all of it\n"), Total: 10}}})
	resp, body := get(t, srv.URL+"/jobs/1/logs")
	if resp.StatusCode != http.StatusOK || body != "all of it\n" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("logs of a done job: %d %q %s", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
}

// TestLogsStreamWhileTheJobRuns: the response stays open while the job
// runs, each flush's new bytes arrive as they are written to the store, and
// the response ends when the job does.
func TestLogsStreamWhileTheJobRuns(t *testing.T) {
	srv, store := newServer(t)
	post(t, srv.URL+"/jobs", `{"args": ["true"]}`)
	store.set(1, queue.Status{State: "running", Result: queue.Result{Log: queue.Log{Tail: []byte("one\n"), Total: 4}}})

	resp, err := http.Get(srv.URL + "/jobs/1/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	read := func(want string) {
		t.Helper()
		buf := make([]byte, 64)
		// EOF beside the last chunk is fine: the job may be done by then.
		n, err := resp.Body.Read(buf)
		if (err != nil && err != io.EOF) || string(buf[:n]) != want {
			t.Fatalf("read %q (%v), want %q", buf[:n], err, want)
		}
	}
	read("one\n")
	store.set(1, queue.Status{State: "running", Result: queue.Result{Log: queue.Log{Tail: []byte("one\ntwo\n"), Total: 8}}})
	read("two\n")
	store.set(1, queue.Status{State: "done", Result: queue.Result{Log: queue.Log{Tail: []byte("one\ntwo\nthree\n"), Total: 14}}})
	read("three\n")
	if rest, err := io.ReadAll(resp.Body); err != nil || len(rest) != 0 {
		t.Fatalf("after the job finished the stream had %q (%v), want it closed", rest, err)
	}
}

// TestLogsSayWhenBytesWereSkipped: the row holds a tail; a follower that
// missed output between reads is told how much rather than shown a splice.
func TestLogsSayWhenBytesWereSkipped(t *testing.T) {
	srv, store := newServer(t)
	post(t, srv.URL+"/jobs", `{"args": ["true"]}`)
	// 100 bytes produced, the last 8 kept.
	store.set(1, queue.Status{State: "done", Result: queue.Result{Log: queue.Log{Tail: []byte("tail-end"), Total: 100}}})
	_, body := get(t, srv.URL+"/jobs/1/logs")
	if body != "\n[forge: 92 bytes of output not kept]\ntail-end" {
		t.Fatalf("logs = %q", body)
	}
}

// TestLogsSayWhenTheJobRestarted: a total below what was already sent is a
// reclaim by another worker, whose output starts from nothing.
func TestLogsSayWhenTheJobRestarted(t *testing.T) {
	srv, store := newServer(t)
	post(t, srv.URL+"/jobs", `{"args": ["true"]}`)
	store.set(1, queue.Status{State: "running", Attempt: 1, Result: queue.Result{Log: queue.Log{Tail: []byte("first try\n"), Total: 10}}})
	resp, err := http.Get(srv.URL + "/jobs/1/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "first try\n" {
		t.Fatalf("first read %q", buf[:n])
	}
	store.set(1, queue.Status{State: "done", Attempt: 2, Result: queue.Result{Log: queue.Log{Tail: []byte("again\n"), Total: 6}}})
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "\n[forge: job restarted, attempt 2]\nagain\n" {
		t.Fatalf("after the restart: %q", rest)
	}
}

func TestMetricsCountTheQueue(t *testing.T) {
	srv, store := newServer(t)
	post(t, srv.URL+"/jobs", `{"args": ["true"]}`)
	post(t, srv.URL+"/jobs", `{"args": ["true"]}`)
	store.set(2, queue.Status{State: "running"})
	_, body := get(t, srv.URL+"/metrics")
	want := "forge_queue_jobs{state=\"queued\"} 1\nforge_queue_jobs{state=\"running\"} 1\nforge_queue_jobs{state=\"done\"} 0\n"
	if !strings.Contains(body, want) {
		t.Fatalf("metrics =\n%s\nwant to contain\n%s", body, want)
	}
}
