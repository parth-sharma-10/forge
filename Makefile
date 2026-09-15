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

.PHONY: all test fmt vet race build clean rootfs test-sandbox vm-test vm-shell mutate

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
	rm -f $(VM_TEST_BIN) $(VM_CGROUP_BIN)

## rootfs: unpack an Alpine root filesystem for the sandbox to run in
rootfs:
	@test -n "$(ALPINE_SHA256)" || { echo "no pinned checksum for $(ALPINE_ARCH)"; exit 1; }
	@if [ -x "$(ROOTFS)/bin/sh" ]; then echo "$(ROOTFS) already unpacked"; exit 0; fi; \
	tmp=$$(mktemp -d) && curl -fsSL "$(ALPINE_URL)" -o "$$tmp/rootfs.tar.gz" && \
	echo "$(ALPINE_SHA256)  $$tmp/rootfs.tar.gz" | sha256sum -c - && \
	sudo mkdir -p "$(ROOTFS)" && sudo tar -xzf "$$tmp/rootfs.tar.gz" -C "$(ROOTFS)" && \
	rm -rf "$$tmp" && echo "unpacked alpine $(ALPINE_VERSION) into $(ROOTFS)"

## test-sandbox: the Linux-only isolation tests, as root (Linux hosts and CI)
# The test binary is built and then run under sudo, rather than running
# `sudo go test`: root has a different GOCACHE and would rebuild the world.
test-sandbox: rootfs
	$(GO) test -c -o /tmp/forge-sandbox.test ./internal/sandbox
	$(GO) test -c -o /tmp/forge-cgroup.test ./internal/cgroup
	sudo FORGE_TEST_ROOTFS=$(ROOTFS) /tmp/forge-sandbox.test -test.v -test.count=1
	sudo /tmp/forge-cgroup.test -test.v -test.count=1

## vm-test: run the sandbox tests inside the colima VM (macOS hosts)
# colima mounts $HOME into the VM at the same path, so the cross-compiled test
# binary needs no copying in and the VM needs no Go toolchain.
vm-test: $(VM_TEST_BIN) $(VM_CGROUP_BIN)
	$(VM) ssh -- sh -euc '\
	  if [ ! -x $(ROOTFS)/bin/sh ]; then \
	    curl -fsSL $(ALPINE_URL) -o /tmp/rootfs.tgz; \
	    echo "$(ALPINE_SHA256)  /tmp/rootfs.tgz" | sha256sum -c -; \
	    sudo mkdir -p $(ROOTFS); sudo tar -xzf /tmp/rootfs.tgz -C $(ROOTFS); \
	  fi; \
	  cp $(VM_TEST_BIN) /tmp/forge-sandbox.test; \
	  cp $(VM_CGROUP_BIN) /tmp/forge-cgroup.test; \
	  sudo FORGE_TEST_ROOTFS=$(ROOTFS) /tmp/forge-sandbox.test -test.v -test.count=1; \
	  sudo /tmp/forge-cgroup.test -test.v -test.count=1'

$(VM_TEST_BIN): $(wildcard internal/sandbox/*.go) $(wildcard internal/cgroup/*.go)
	@mkdir -p $(dir $@)
	GOOS=linux GOARCH=$(VM_GOARCH) $(GO) test -c -o $@ ./internal/sandbox

$(VM_CGROUP_BIN): $(wildcard internal/cgroup/*.go)
	@mkdir -p $(dir $@)
	GOOS=linux GOARCH=$(VM_GOARCH) $(GO) test -c -o $@ ./internal/cgroup

vm-shell:
	$(VM) ssh

## mutate: break the sandbox one property at a time and check the tests notice
# Runs inside the VM like vm-test. Exits non-zero on any mutation the suite
# still passes; the score in docs/phase-0.md comes from this and nowhere else.
mutate:
	VM=$(VM) FORGE_TEST_ROOTFS=$(ROOTFS) hack/mutate.py
