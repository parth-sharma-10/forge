//go:build linux

package syscalls

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"

	"github.com/parth-sharma-10/forge/internal/metrics"
)

const (
	// maxJobs bounds the enrolled map: jobs observed at once on one worker.
	maxJobs = 1024
	// maxCounts bounds the counts map: (cgroup, syscall) pairs held between
	// drains. A full map drops new pairs — a count is lost, nothing worse.
	maxCounts = 1 << 16
)

// Observer holds the loaded program, its maps and the link keeping it
// attached. It is safe for concurrent use: several workers drain different
// jobs at once.
type Observer struct {
	enrolled *ebpf.Map
	counts   *ebpf.Map
	prog     *ebpf.Program
	link     link.Link
	// exitProg and exitLink count ENOSYS returns; nil where the sys_exit
	// tracepoint could not be attached, which leaves the histogram working.
	exitProg *ebpf.Program
	exitLink link.Link
	log      *slog.Logger

	reg *metrics.Registry
	mu  sync.Mutex
	// counters memoises one *metrics.Counter per counts-map key number (the
	// syscall number, tagged with enosysTag for an ENOSYS return) so a drain
	// adds to the same series the last drain created.
	counters map[uint64]*metrics.Counter
}

// countsKey mirrors the 16-byte BPF map key: two little-endian u64s, cgroup id
// then syscall number, in the layout program_linux.go stores.
type countsKey struct{ Cgroup, Nr uint64 }

// Load loads and attaches the observation program. On any failure — no
// capability to load BPF, a kernel without the features, a non-Linux build —
// it returns an error the worker treats as "run without observability".
func Load(reg *metrics.Registry, log *slog.Logger) (*Observer, error) {
	// Since Linux 5.11 BPF objects are charged to the memory cgroup and this
	// is a no-op; on older kernels it lifts RLIMIT_MEMLOCK. Best effort: a
	// failure here should not be the reason observability is off.
	_ = rlimit.RemoveMemlock()

	enrolled, err := ebpf.NewMap(&ebpf.MapSpec{
		Type: ebpf.Hash, KeySize: 8, ValueSize: 8, MaxEntries: maxJobs,
	})
	if err != nil {
		return nil, fmt.Errorf("syscalls: enrolled map: %w", err)
	}
	counts, err := ebpf.NewMap(&ebpf.MapSpec{
		Type: ebpf.PerCPUHash, KeySize: 16, ValueSize: 8, MaxEntries: maxCounts,
	})
	if err != nil {
		enrolled.Close()
		return nil, fmt.Errorf("syscalls: counts map: %w", err)
	}
	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:         "forge_syscalls",
		Type:         ebpf.RawTracepoint,
		Instructions: buildProgram(enrolled, counts),
		// The declared program licence gates which BPF helpers the kernel will
		// let it call; it is not Forge's licence and does not relicense it.
		License: "GPL",
	})
	if err != nil {
		counts.Close()
		enrolled.Close()
		return nil, fmt.Errorf("syscalls: load program: %w", err)
	}
	lnk, err := link.AttachRawTracepoint(link.RawTracepointOptions{Name: "sys_enter", Program: prog})
	if err != nil {
		prog.Close()
		counts.Close()
		enrolled.Close()
		return nil, fmt.Errorf("syscalls: attach sys_enter: %w", err)
	}
	o := &Observer{
		enrolled: enrolled, counts: counts, prog: prog, link: lnk,
		log: log, reg: reg, counters: map[uint64]*metrics.Counter{},
	}
	if err := o.attachExit(); err != nil {
		log.Warn("syscall observability: ENOSYS counting off, the histogram is unaffected", "err", err)
	}
	return o, nil
}

// attachExit loads and attaches the ENOSYS counter. Its failure is logged and
// survived: the sys_enter histogram does not depend on it.
func (o *Observer) attachExit() error {
	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:         "forge_enosys",
		Type:         ebpf.TracePoint,
		Instructions: buildExitProgram(o.enrolled, o.counts),
		License:      "GPL",
	})
	if err != nil {
		return fmt.Errorf("syscalls: load exit program: %w", err)
	}
	lnk, err := link.Tracepoint("raw_syscalls", "sys_exit", prog, nil)
	if err != nil {
		prog.Close()
		return fmt.Errorf("syscalls: attach raw_syscalls:sys_exit: %w", err)
	}
	o.exitProg, o.exitLink = prog, lnk
	return nil
}

// Job returns the sandbox.Spec.Observe callback: it enrols a job's cgroup on
// call and drains its counts when the returned func runs, after the build has
// exited. Enrolment that fails leaves the job unobserved rather than unbuilt.
func (o *Observer) Job() func(cgroupID uint64) (done func()) {
	return func(cgroupID uint64) func() {
		if err := o.enrolled.Put(cgroupID, uint64(1)); err != nil {
			o.log.Warn("syscall observability: enrol failed, job runs unobserved",
				"cgroup", cgroupID, "err", err)
			return nil
		}
		return func() { o.drain(cgroupID) }
	}
}

// drain reads out one job's counts, adds them to the metrics, and clears them.
// It runs after the build has exited, so no new counts can arrive for this
// cgroup; the enrolment is removed first regardless.
//
// ponytail: drain scans the whole counts map filtering by cgroup id, O(keys)
// per job; a per-cgroup map-of-maps if concurrent job counts climb enough to
// show up.
func (o *Observer) drain(cgroupID uint64) {
	if err := o.enrolled.Delete(cgroupID); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		o.log.Warn("syscall observability: un-enrol failed", "cgroup", cgroupID, "err", err)
	}

	var (
		key    countsKey
		perCPU []uint64 // one entry per possible CPU; cilium sizes it.
		drop   []countsKey
	)
	it := o.counts.Iterate()
	for it.Next(&key, &perCPU) {
		if key.Cgroup != cgroupID {
			continue
		}
		var sum int64
		for _, v := range perCPU {
			sum += int64(v)
		}
		if sum > 0 {
			o.counter(key.Nr).Add(sum)
		}
		drop = append(drop, key)
	}
	if err := it.Err(); err != nil {
		o.log.Warn("syscall observability: drain scan", "cgroup", cgroupID, "err", err)
	}
	for _, k := range drop {
		if err := o.counts.Delete(k); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			o.log.Warn("syscall observability: clear count", "cgroup", k.Cgroup, "nr", k.Nr, "err", err)
		}
	}
}

// counter returns the counter for a counts-map key number, creating it once:
// the syscall histogram's, or the ENOSYS count's for a tagged key.
func (o *Observer) counter(key uint64) *metrics.Counter {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.counters[key]
	if c == nil {
		name, help := MetricName, "Syscalls made by builds, by number under the build's architecture."
		if key&enosysTag != 0 {
			name, help = EnosysMetricName, "Syscalls by builds that returned ENOSYS: denied by the seccomp filter, or not implemented."
		}
		c = o.reg.Counter(name, help, map[string]string{"nr": strconv.FormatUint(key&^enosysTag, 10)})
		o.counters[key] = c
	}
	return c
}

// Close detaches the program and releases the maps.
func (o *Observer) Close() error {
	err := o.link.Close()
	o.prog.Close()
	if o.exitLink != nil {
		err = errors.Join(err, o.exitLink.Close())
		o.exitProg.Close()
	}
	o.counts.Close()
	o.enrolled.Close()
	return err
}
