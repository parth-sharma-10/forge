//go:build linux

package sandbox

import (
	"context"
	"io"
	"testing"
)

// BenchmarkRun is the sandbox's setup cost: clone into six namespaces, the
// overlay, /proc and /dev, pivot_root, the bounding set, the filter, an
// exec of /bin/true, and the teardown of all of it. Root and an image, like
// the tests; `make bench`.
func BenchmarkRun(b *testing.B) {
	dir := benchRootfs(b)
	for i := 0; i < b.N; i++ {
		if err := Run(context.Background(), Spec{Rootfs: dir, Args: []string{"/bin/true"}}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRunWithLimits adds a cgroup per job, which is the shape every
// job through the worker has.
func BenchmarkRunWithLimits(b *testing.B) {
	dir := benchRootfs(b)
	for i := 0; i < b.N; i++ {
		spec := Spec{Rootfs: dir, Args: []string{"/bin/true"}}
		spec.Limits.MemoryBytes = 64 << 20
		spec.Limits.MaxPIDs = 32
		if err := Run(context.Background(), spec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRunSupervised is BenchmarkRun with artifacts collected: the build
// runs under the supervisor, which forks it, reaps it, kills what is left and
// walks an /out that does not exist. The difference from BenchmarkRun is the
// supervisor's cost on a job that hands back nothing.
func BenchmarkRunSupervised(b *testing.B) {
	dir := benchRootfs(b)
	for i := 0; i < b.N; i++ {
		spec := Spec{Rootfs: dir, Args: []string{"/bin/true"}, Artifacts: io.Discard, ArtifactBytes: 16 << 20}
		if err := Run(context.Background(), spec); err != nil {
			b.Fatal(err)
		}
	}
}
