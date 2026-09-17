//go:build linux

package sandbox

import (
	"context"
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
