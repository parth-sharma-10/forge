// Package metrics is the Prometheus text format, written by hand: a counter
// is a name, a help line, a type line and a number, and a client library is
// for the histograms and pushes Forge does not have.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter only goes up.
type Counter struct{ n atomic.Int64 }

func (c *Counter) Inc()         { c.n.Add(1) }
func (c *Counter) Value() int64 { return c.n.Load() }

// Gauge goes both ways.
type Gauge struct{ n atomic.Int64 }

func (g *Gauge) Inc()         { g.n.Add(1) }
func (g *Gauge) Dec()         { g.n.Add(-1) }
func (g *Gauge) Set(v int64)  { g.n.Store(v) }
func (g *Gauge) Value() int64 { return g.n.Load() }

// Registry is a set of named metrics that writes itself out. A metric may
// be registered under a name once per label set; the labels are part of
// the name as Prometheus reads it, `name{k="v"}`.
type Registry struct {
	mu      sync.Mutex
	entries []entry
	// Sampled are read at write time — for a value that lives elsewhere,
	// such as a count in the database.
	sampled []sampled
}

type entry struct {
	name, help, kind string
	labels           string
	value            func() int64
}

type sampled struct {
	name, help, kind string
	sample           func() ([]Sample, error)
}

// Sample is one labelled value of a sampled metric.
type Sample struct {
	Labels map[string]string
	Value  int64
}

// Counter registers a counter under name with labels and returns it.
func (r *Registry) Counter(name, help string, labels map[string]string) *Counter {
	c := &Counter{}
	r.add(name, help, "counter", labels, c.Value)
	return c
}

// Gauge registers a gauge under name with labels and returns it.
func (r *Registry) Gauge(name, help string, labels map[string]string) *Gauge {
	g := &Gauge{}
	r.add(name, help, "gauge", labels, g.Value)
	return g
}

// Sampled registers a gauge whose values are produced on every scrape.
func (r *Registry) Sampled(name, help string, sample func() ([]Sample, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sampled = append(r.sampled, sampled{name, help, "gauge", sample})
}

func (r *Registry) add(name, help, kind string, labels map[string]string, value func() int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry{name, help, kind, formatLabels(labels), value})
}

// formatLabels renders labels sorted by key, so output is stable.
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, labels[k])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// WriteTo writes every metric in the exposition format. Metrics that
// share a name share one HELP and TYPE line, as the format requires.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out strings.Builder
	described := map[string]bool{}
	for _, e := range r.entries {
		if !described[e.name] {
			fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n", e.name, e.help, e.name, e.kind)
			described[e.name] = true
		}
		fmt.Fprintf(&out, "%s%s %d\n", e.name, e.labels, e.value())
	}
	for _, s := range r.sampled {
		samples, err := s.sample()
		if err != nil {
			// A scrape that cannot read one value still reports the rest;
			// the failure is visible as the metric's absence.
			fmt.Fprintf(&out, "# %s: %v\n", s.name, err)
			continue
		}
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n", s.name, s.help, s.name, s.kind)
		for _, v := range samples {
			fmt.Fprintf(&out, "%s%s %d\n", s.name, formatLabels(v.Labels), v.Value)
		}
	}
	n, err := io.WriteString(w, out.String())
	return int64(n), err
}

// ServeHTTP is the /metrics handler.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	r.WriteTo(w)
}
