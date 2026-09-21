//go:build !linux

package syscalls

import (
	"log/slog"

	"github.com/parth-sharma-10/forge/internal/metrics"
)

// Observer is the no-op stand-in on platforms without eBPF. It exists so the
// worker and its command compile everywhere; Load never returns one.
type Observer struct{}

// Load reports that observability is unavailable off Linux.
func Load(*metrics.Registry, *slog.Logger) (*Observer, error) { return nil, ErrUnsupported }

// Job is never called: Load does not return an Observer here.
func (o *Observer) Job() func(cgroupID uint64) (done func()) { return nil }

// Close is a no-op.
func (o *Observer) Close() error { return nil }
