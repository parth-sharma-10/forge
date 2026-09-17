package metrics

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExpositionFormat(t *testing.T) {
	var r Registry
	ok := r.Counter("forge_jobs_done_total", "Jobs finished, by outcome.", map[string]string{"outcome": "ok"})
	failed := r.Counter("forge_jobs_done_total", "Jobs finished, by outcome.", map[string]string{"outcome": "failed"})
	running := r.Gauge("forge_jobs_running", "Jobs in hand.", nil)
	r.Sampled("forge_queue_jobs", "Jobs in the queue, by state.", func() ([]Sample, error) {
		return []Sample{{map[string]string{"state": "queued"}, 4}}, nil
	})
	ok.Inc()
	ok.Inc()
	failed.Inc()
	running.Inc()
	running.Inc()
	running.Dec()

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, nil)
	want := `# HELP forge_jobs_done_total Jobs finished, by outcome.
# TYPE forge_jobs_done_total counter
forge_jobs_done_total{outcome="ok"} 2
forge_jobs_done_total{outcome="failed"} 1
# HELP forge_jobs_running Jobs in hand.
# TYPE forge_jobs_running gauge
forge_jobs_running 1
# HELP forge_queue_jobs Jobs in the queue, by state.
# TYPE forge_queue_jobs gauge
forge_queue_jobs{state="queued"} 4
`
	if got := rec.Body.String(); got != want {
		t.Fatalf("exposition =\n%s\nwant\n%s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// TestASampleThatFailsIsAbsentNotFatal: the database being down must not
// take the process's own counters with it.
func TestASampleThatFailsIsAbsentNotFatal(t *testing.T) {
	var r Registry
	c := r.Counter("up", "still here", nil)
	c.Inc()
	r.Sampled("depth", "from the database", func() ([]Sample, error) { return nil, errors.New("connection refused") })
	var b strings.Builder
	r.WriteTo(&b)
	got := b.String()
	if !strings.Contains(got, "up 1\n") || strings.Contains(got, "# TYPE depth") || !strings.Contains(got, "# depth: connection refused") {
		t.Fatalf("exposition with a failing sample:\n%s", got)
	}
}

func TestLabelsAreSortedByKey(t *testing.T) {
	if got := formatLabels(map[string]string{"z": "1", "a": "2"}); got != `{a="2",z="1"}` {
		t.Fatalf("labels = %s", got)
	}
}
