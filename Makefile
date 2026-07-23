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

.PHONY: build test lint verify

build:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/agentbridge ./cmd/agentbridge

test:
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) test ./...

lint:
	@test -z "$$($(GOFMT) -l .)" || { $(GOFMT) -d .; exit 1; }
	GOTOOLCHAIN=$(GOTOOLCHAIN) $(GO) vet ./...

verify:
	bash scripts/verify.sh
