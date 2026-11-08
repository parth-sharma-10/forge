GO ?= go
PKGS ?= ./...

# The sandbox tests need a root filesystem to pivot into, a Linux kernel, and
# CAP_SYS_ADMIN. ROOTFS is deliberately outside the repo: it holds setuid
# binaries and gets bind-mounted.
ROOTFS ?= /var/tmp/forge-rootfs
ALPINE_VERSION ?= 3.21.3
ALPINE_ARCH ?= $(shell uname -m | sed 's/^arm64$$/aarch64/')
ALPINE_URL = https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/$(ALPINE_ARCH)/alpine-minirootfs-$(ALPINE_VERSION)-$(ALPINE_ARCH).tar.gz
# Pinned rather than taken from the mirror's own .sha256, which a compromised
# mirror would also serve. This is code Forge unpacks and executes as root.
ALPINE_SHA256_aarch64 = ead8a4b37867bd19e7417dd078748e2312c0aea364403d96758d63ea8ff261ea
ALPINE_SHA256_x86_64 = 1a694899e406ce55d32334c47ac0b2efb6c06d7e878102d1840892ad44cd5239
ALPINE_SHA256 = $(ALPINE_SHA256_$(ALPINE_ARCH))

VM ?= colima
VM_GOARCH = $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
VM_TEST_BIN = $(HOME)/.cache/forge/sandbox.test
VM_CGROUP_BIN = $(HOME)/.cache/forge/cgroup.test
VM_WORKER_BIN = $(HOME)/.cache/forge/worker.test

# The queue tests and the worker's end-to-end test need a PostgreSQL to talk
# to, named by FORGE_TEST_DSN; without it they skip on a developer's machine
# and fail in CI. VM_DSN is the one inside the colima VM, for vm-test.
FORGE_TEST_DSN ?=
VM_DSN ?= postgres://forge:forge@127.0.0.1:5432/forge

.PHONY: all test fmt vet race build clean rootfs test-sandbox test-syscalls vm-test vm-shell mutate mutate-queue vm-mutate-queue fuzz bench vm-bench verify-units

all: test

## test: formatting check, vet, and the race-enabled test suite
test: fmt vet race

fmt:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet $(PKGS)

race:
	$(GO) test -race -count=1 $(PKGS)

build:
	$(GO) build ./...

clean:
	$(GO) clean -testcache
	rm -f $(VM_TEST_BIN) $(VM_CGROUP_BIN) $(VM_WORKER_BIN)

## rootfs: unpack an Alpine root filesystem for the sandbox to run in
# The files are then shifted into the sandbox's uid map, which the overlay
# needs in order to copy them up; see hack/shift-rootfs.sh. Re-running only
# repeats the shift, which is idempotent, so an image unpacked before the
# overlay existed is brought up to date by the same target.
rootfs:
	@test -n "$(ALPINE_SHA256)" || { echo "no pinned checksum for $(ALPINE_ARCH)"; exit 1; }
	@if [ -x "$(ROOTFS)/bin/sh" ]; then echo "$(ROOTFS) already unpacked"; else \
	tmp=$$(mktemp -d) && curl -fsSL "$(ALPINE_URL)" -o "$$tmp/rootfs.tar.gz" && \
	echo "$(ALPINE_SHA256)  $$tmp/rootfs.tar.gz" | sha256sum -c - && \
	sudo mkdir -p "$(ROOTFS)" && sudo tar -xzf "$$tmp/rootfs.tar.gz" -C "$(ROOTFS)" && \
	rm -rf "$$tmp" && echo "unpacked alpine $(ALPINE_VERSION) into $(ROOTFS)"; fi
	sudo hack/shift-rootfs.sh "$(ROOTFS)"

## test-sandbox: the Linux-only isolation tests, as root (Linux hosts and CI)
# The test binary is built and then run under sudo, rather than running
# `sudo go test`: root has a different GOCACHE and would rebuild the world.
#
# The sandbox test binary is built without cgo: some tests copy it into the
# Alpine image and run it there as the build, and a binary linked against the
# host's glibc has no loader to start in a musl image (execve says ENOENT).
# archive/tar imports os/user, which links libc whenever cgo is on.
test-sandbox: rootfs
	CGO_ENABLED=0 $(GO) test -c -o /tmp/forge-sandbox.test ./internal/sandbox
	$(GO) test -c -o /tmp/forge-cgroup.test ./internal/cgroup
	$(GO) test -c -o /tmp/forge-worker.test ./internal/worker
	sudo FORGE_TEST_ROOTFS=$(ROOTFS) /tmp/forge-sandbox.test -test.v -test.count=1
	sudo /tmp/forge-cgroup.test -test.v -test.count=1
	sudo FORGE_TEST_ROOTFS=$(ROOTFS) FORGE_TEST_DSN=$(FORGE_TEST_DSN) CI=$(CI) /tmp/forge-worker.test -test.v -test.count=1 -test.run TestEndToEnd

## test-syscalls: the eBPF syscall-observability test, as root (Linux hosts and CI)
# Separate from test-sandbox because it needs CAP_BPF and a raw_tracepoint on
# top of CAP_SYS_ADMIN, and it loads a real BPF program into the running
# kernel. It runs two sandboxed builds and checks each job's syscalls are
# attributed to its own cgroup.
test-syscalls: rootfs
	$(GO) test -c -o /tmp/forge-syscalls.test ./internal/syscalls
	sudo FORGE_TEST_ROOTFS=$(ROOTFS) CI=$(CI) /tmp/forge-syscalls.test -test.v -test.count=1

## vm-test: run the sandbox tests inside the colima VM (macOS hosts)
# colima mounts $HOME into the VM at the same path, so the cross-compiled test
# binary needs no copying in and the VM needs no Go toolchain.
vm-test: $(VM_TEST_BIN) $(VM_CGROUP_BIN) $(VM_WORKER_BIN)
	$(VM) ssh -- sh -euc '\
	  if [ ! -x $(ROOTFS)/bin/sh ]; then \
	    curl -fsSL $(ALPINE_URL) -o /tmp/rootfs.tgz; \
	    echo "$(ALPINE_SHA256)  /tmp/rootfs.tgz" | sha256sum -c -; \
	    sudo mkdir -p $(ROOTFS); sudo tar -xzf /tmp/rootfs.tgz -C $(ROOTFS); \
	  fi; \
	  sudo $(CURDIR)/hack/shift-rootfs.sh $(ROOTFS); \
	  cp $(VM_TEST_BIN) /tmp/forge-sandbox.test; \
	  cp $(VM_CGROUP_BIN) /tmp/forge-cgroup.test; \
	  cp $(VM_WORKER_BIN) /tmp/forge-worker.test; \
	  sudo FORGE_TEST_ROOTFS=$(ROOTFS) /tmp/forge-sandbox.test -test.v -test.count=1; \
	  sudo /tmp/forge-cgroup.test -test.v -test.count=1; \
	  sudo FORGE_TEST_ROOTFS=$(ROOTFS) FORGE_TEST_DSN=$(VM_DSN) /tmp/forge-worker.test -test.v -test.count=1 -test.run TestEndToEnd'

$(VM_TEST_BIN): $(wildcard internal/sandbox/*.go) $(wildcard internal/cgroup/*.go)
	@mkdir -p $(dir $@)
	GOOS=linux GOARCH=$(VM_GOARCH) $(GO) test -c -o $@ ./internal/sandbox

$(VM_CGROUP_BIN): $(wildcard internal/cgroup/*.go)
	@mkdir -p $(dir $@)
	GOOS=linux GOARCH=$(VM_GOARCH) $(GO) test -c -o $@ ./internal/cgroup

$(VM_WORKER_BIN): $(wildcard internal/*/*.go)
	@mkdir -p $(dir $@)
	GOOS=linux GOARCH=$(VM_GOARCH) $(GO) test -c -o $@ ./internal/worker

vm-shell:
	$(VM) ssh

## mutate: break the sandbox one property at a time and check the tests notice
# Runs inside the VM like vm-test. Exits non-zero on any mutation the suite
# still passes; the score in docs/phase-0.md comes from this and nowhere else.
mutate:
	VM="$(VM)" FORGE_TEST_ROOTFS=$(ROOTFS) hack/mutate.py

## mutate-queue: the same for the queue, on the host, against FORGE_TEST_DSN
mutate-queue:
	FORGE_TEST_DSN=$(FORGE_TEST_DSN) hack/mutate.py --queue

## vm-mutate-queue: the same, inside the VM against VM_DSN, where tests and database share a clock
vm-mutate-queue:
	VM="$(VM)" VM_DSN=$(VM_DSN) FORGE_TEST_ROOTFS=$(ROOTFS) hack/mutate.py --queue-vm

## fuzz: explore the seccomp filter against its policy oracle (Linux, Go, no root)
# The fuzz target is pure functions — the filter and the evaluate interpreter —
# so unlike the rest of the sandbox tests it needs neither privileges nor a
# rootfs, only a Go toolchain on Linux (the package is Linux-only). The seed
# corpus runs in the ordinary suite; this is the exploring form. Any escape is
# written to internal/sandbox/testdata/fuzz as a permanent regression seed.
FUZZTIME ?= 60s
fuzz:
	$(GO) test -run '^$$' -fuzz='^FuzzFilterMatchesPolicy$$' -fuzztime=$(FUZZTIME) ./internal/sandbox

## bench: sandbox setup latency and jobs per second (Linux, root, image, database)
# The test binaries from test-sandbox, run for their benchmarks only. The
# numbers in the README come from this target's step in CI, on the runner it names.
BENCH_ARGS = -test.run '^$$' -test.bench . -test.benchtime 20x -test.count=1
bench: rootfs
	$(GO) test -c -o /tmp/forge-sandbox.test ./internal/sandbox
	$(GO) test -c -o /tmp/forge-worker.test ./internal/worker
	sudo FORGE_TEST_ROOTFS=$(ROOTFS) /tmp/forge-sandbox.test $(BENCH_ARGS)
	sudo FORGE_TEST_ROOTFS=$(ROOTFS) FORGE_TEST_DSN=$(FORGE_TEST_DSN) /tmp/forge-worker.test $(BENCH_ARGS)

## vm-bench: bench, from a macOS host, inside the colima VM
vm-bench: $(VM_TEST_BIN) $(VM_WORKER_BIN)
	$(VM) ssh -- sh -euc '\
	  cp $(VM_TEST_BIN) /tmp/forge-sandbox.test; \
	  cp $(VM_WORKER_BIN) /tmp/forge-worker.test; \
	  uname -mr; nproc; \
	  sudo FORGE_TEST_ROOTFS=$(ROOTFS) /tmp/forge-sandbox.test $(BENCH_ARGS); \
	  sudo FORGE_TEST_ROOTFS=$(ROOTFS) FORGE_TEST_DSN=$(VM_DSN) /tmp/forge-worker.test $(BENCH_ARGS)'

## verify-units: the systemd units in deploy/ parse, with the binaries where they say
verify-units: build
	$(GO) build -o /tmp/forge-api ./cmd/forge-api
	$(GO) build -o /tmp/forge-worker ./cmd/forge-worker
	sudo install -m 0755 /tmp/forge-api /tmp/forge-worker /usr/local/bin/
	systemd-analyze verify deploy/forge-api.service deploy/forge-worker.service
