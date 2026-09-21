package syscalls

import (
	"io"
	"log/slog"
	"testing"

	"github.com/parth-sharma-10/forge/internal/metrics"
)

// TestLoadNeverReturnsABrokenObserver: Load either returns a usable Observer
// or an error and no Observer, never a half-built one. This is the invariant
// the worker's graceful degradation rests on, and it holds everywhere — off
// Linux, and on a Linux host without the capability to load BPF.
func TestLoadNeverReturnsABrokenObserver(t *testing.T) {
	var reg metrics.Registry
	obs, err := Load(&reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		if obs != nil {
			t.Fatalf("Load returned an error and a non-nil Observer: %v", err)
		}
		return
	}
	obs.Close()
}
