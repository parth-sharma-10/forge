package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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
	args map[int64][]string
}

func newStore() *fakeStore {
	return &fakeStore{jobs: map[int64]queue.Status{}, args: map[int64][]string{}}
}

func (s *fakeStore) Submit(_ context.Context, args []string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := int64(len(s.jobs) + 1)
	s.jobs[id] = queue.Status{State: "queued"}
	s.args[id] = args
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

func (s *fakeStore) Cancel(_ context.Context, id int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.jobs[id]
	switch {
	case !ok:
		return "", queue.ErrNotFound
	case st.State == "done":
		return "", queue.ErrDone
	case st.State == "queued":
		st.State, st.Result.ExitCode, st.Result.Error = "done", -1, queue.ReasonCancelled
	}
	st.Cancelled = true
	s.jobs[id] = st
	return st.State, nil
}

func (s *fakeStore) List(_ context.Context, state string, before int64, limit int) ([]queue.Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []queue.Summary
	for id, st := range s.jobs {
		if (state == "" || st.State == state) && (before == 0 || id < before) {
			out = append(out, queue.Summary{ID: id, Args: s.args[id], Status: st})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out[:min(limit, len(out))], nil
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

// TestCancel: a queued job is finished at once (200), a running one is marked
// for its worker (202), a done one is left alone (409), an unknown one is 404.
func TestCancel(t *testing.T) {
	srv, store := newServer(t)
	post(t, srv.URL+"/jobs", `{"args": ["true"]}`)
	post(t, srv.URL+"/jobs", `{"args": ["true"]}`)
	post(t, srv.URL+"/jobs", `{"args": ["true"]}`)
	store.set(2, queue.Status{State: "running", Attempt: 1})
	store.set(3, queue.Status{State: "done", Attempt: 1})

	for _, c := range []struct {
		path string
		want int
	}{
		{"/jobs/1/cancel", http.StatusOK},
		{"/jobs/2/cancel", http.StatusAccepted},
		{"/jobs/3/cancel", http.StatusConflict},
		{"/jobs/42/cancel", http.StatusNotFound},
		{"/jobs/abc/cancel", http.StatusBadRequest},
	} {
		resp := post(t, srv.URL+c.path, "")
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("POST %s: %d, want %d", c.path, resp.StatusCode, c.want)
		}
	}

	_, body := get(t, srv.URL+"/jobs/1")
	if !strings.Contains(body, `"state":"done"`) || !strings.Contains(body, `"cancel_requested":true`) ||
		!strings.Contains(body, `"error":"cancelled"`) {
		t.Errorf("a cancelled queued job reads %s", body)
	}
	_, body = get(t, srv.URL+"/jobs/2")
	if !strings.Contains(body, `"state":"running"`) || !strings.Contains(body, `"cancel_requested":true`) {
		t.Errorf("a cancelled running job reads %s", body)
	}
}

func TestListPagesNewestFirst(t *testing.T) {
	srv, store := newServer(t)
	for _, cmd := range []string{"a", "b", "c"} {
		post(t, srv.URL+"/jobs", `{"args": ["`+cmd+`"]}`)
	}
	store.set(1, queue.Status{State: "done", Result: queue.Result{ExitCode: 0}})

	type page struct {
		Jobs []Job
		Next int64
	}
	read := func(path string) page {
		t.Helper()
		resp, body := get(t, srv.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
		}
		var p page
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	p := read("/jobs?limit=2")
	if len(p.Jobs) != 2 || p.Jobs[0].ID != 3 || p.Jobs[1].ID != 2 || p.Next != 2 || p.Jobs[0].Args[0] != "c" {
		t.Fatalf("first page = %+v, want jobs 3, 2 with next 2", p)
	}
	p = read("/jobs?limit=2&before=2")
	if len(p.Jobs) != 1 || p.Jobs[0].ID != 1 || p.Next != 0 || p.Jobs[0].ExitCode == nil {
		t.Fatalf("last page = %+v, want job 1, done, with no next", p)
	}
	if p = read("/jobs?state=queued"); len(p.Jobs) != 2 {
		t.Fatalf("queued = %+v, want 2", p)
	}
	if p = read("/jobs?state=running"); p.Jobs == nil || len(p.Jobs) != 0 {
		t.Fatalf("running = %+v, want an empty list, not null", p)
	}
}

func TestListRejectsBadQueries(t *testing.T) {
	srv, _ := newServer(t)
	for _, q := range []string{"state=lost", "limit=0", "limit=501", "limit=x", "before=0", "before=x"} {
		if resp, _ := get(t, srv.URL+"/jobs?"+q); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("GET /jobs?%s: %d, want 400", q, resp.StatusCode)
		}
	}
}
