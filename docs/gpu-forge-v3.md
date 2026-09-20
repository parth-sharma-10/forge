# GPU passthrough (Forge v3) — a security analysis, not yet a feature

Status: **analysed, deliberately not built.** There is no GPU on the current
host, and GPU passthrough is a security-sensitive capability that weakens
boundaries the threat model currently holds. It is written down so the eventual
work starts from the trade-offs rather than from a device node.

Do not treat GPU passthrough as "add a device and a flag". It is the largest
single erosion of Forge's isolation surface on the roadmap.

## What a build would need, and what each costs

| Requirement | Boundary it touches | Cost |
|---|---|---|
| `/dev/nvidia0`, `/dev/nvidiactl`, `/dev/nvidia-uvm` (+ `-modeset`) in the sandbox | The **six-device invariant** ([threat-model.md](threat-model.md) boundary 10, `TestDevIsOnlyTheMinimalSet`) | The sandbox's whole `/dev` story is "exactly six character devices, each asserted by major:minor, nothing of the host wholesale". GPU nodes are host devices with direct hardware access. This is the invariant, broken by design. |
| A device-cgroup allow for those nodes | cgroup device control | Today the sandbox needs no device-cgroup rules because it makes no nodes. GPU access needs explicit `devices` allow entries — new policy surface. |
| `ioctl` on the GPU character devices | The **seccomp allowlist** | `ioctl` is already allowed, but the GPU driver's `ioctl` interface is enormous and effectively opaque — it is a direct line to a large, closed kernel/firmware surface. This is the single biggest expansion of *reachable* kernel attack surface Forge would ever make, and the fuzzer cannot bound it: `ioctl` is filtered by number only, not by its request argument. |
| The NVIDIA container runtime stack, or its device/library injection done by hand | New dependency / new privileged host component | The usual path (`nvidia-container-toolkit`) is a large privileged component that mounts host driver libraries into the container. Doing it by hand means tracking driver/userspace version coupling. |

## The honest security position

GPU passthrough moves a build from "no host device, deny-by-default kernel
surface" to "direct `ioctl` access to a proprietary driver and the hardware
behind it". The threat model already names **kernel exploits through an allowed
syscall** as the class Forge does not defend against; GPU passthrough makes that
class dramatically larger and concentrates it in `ioctl`, which the allowlist
cannot meaningfully constrain. A GPU-passthrough Forge is a weaker sandbox than
today's, and the honest framing is "how much isolation are we spending, for what
workload", not "a new feature".

## Options, strongest isolation first

1. **Do not passthrough; schedule GPU work to a dedicated, single-tenant host**
   outside the shared sandbox. Keeps the sandbox's guarantees intact for
   everything else. The right default until there is a concrete need.
2. **MIG (Multi-Instance GPU) partitions** — hardware-enforced isolation
   between tenants on one card. Still exposes the driver `ioctl` surface, but
   contains cross-tenant memory access. Needs specific NVIDIA hardware.
3. **Time-sliced full-device passthrough** — one build owns the whole device
   for its run; simplest to reason about for a single tenant at a time, no
   cross-tenant guarantee within overlapping runs.
4. **Full concurrent passthrough** — weakest; multiple untrusted builds share a
   device and its driver state. Not acceptable without (2).

## What is *not* being built now, and why

- No `/dev/nvidia*` code, no device-cgroup entries, no NVIDIA-stack dependency —
  all speculative until there is GPU hardware and a decided isolation model, and
  Forge does not add speculative surface (`CLAUDE.md`).
- **No faked hardware verification.** Any GPU claim must be measured on real
  hardware; there is none here, so there is nothing to measure and nothing is
  asserted.

## Prerequisites before any implementation

1. GPU hardware, single-tenant, that can be wiped — never the shared host.
2. A decided isolation model (option 1–4 above) with its cross-tenant guarantee
   stated.
3. A revised threat model: which boundaries GPU access spends, and the new
   `ioctl` surface written down as explicitly outside what the allowlist can
   constrain.
4. A decision on the NVIDIA-stack dependency, the same owner's call as the eBPF
   loader ([observability-ebpf.md](observability-ebpf.md)).

Until those exist, GPU passthrough stays a document.
