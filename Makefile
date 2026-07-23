GO ?= go
GOTOOLCHAIN ?= go1.26.4
GOFMT := $(shell GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) env GOROOT)/bin/gofmt

VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell git show -s --format=%cI HEAD 2>/dev/null || printf unknown)
LDFLAGS := -s -w \
	-X github.com/berkayahi/agentbridge/internal/buildinfo.Version=$(VERSION) \
	-X github.com/berkayahi/agentbridge/internal/buildinfo.Commit=$(COMMIT) \
	-X github.com/berkayahi/agentbridge/internal/buildinfo.Date=$(BUILD_DATE)

.PHONY: build test lint verify proto proto-check

build:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/agentbridge ./cmd/agentbridge

test:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) test ./...

lint:
	@test -z "$$($(GOFMT) -l .)" || { $(GOFMT) -d .; exit 1; }
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) vet ./...

verify:
	bash scripts/verify.sh

proto:
	@command -v buf >/dev/null || { echo "buf is required; install the pinned Buf toolchain" >&2; exit 1; }
	cd protocol && buf dep update && buf generate

proto-check:
	@test -f protocol/VERSION && test "$$(tr -d '[:space:]' < protocol/VERSION)" = "1.0.0"
	@command -v buf >/dev/null || { echo "buf is required; install the pinned Buf toolchain" >&2; exit 1; }
	cd protocol && buf lint
	@stable="$$(git tag -l 'protocol/v[0-9]*' | sort -V | tail -1)"; \
	if test -z "$$stable"; then echo "bootstrap baseline: committed v1 schema"; \
	else echo "checking compatibility against $$stable"; git diff --exit-code "$$stable" -- protocol; fi
