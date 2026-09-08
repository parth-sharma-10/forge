GO ?= go
PKGS ?= ./...

.PHONY: all test fmt vet race build clean

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
