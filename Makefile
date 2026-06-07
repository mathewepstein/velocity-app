# Velocity — development tasks.
#
# Common targets:
#   make              Build ./velocity in the repo root.
#   make install      Install to $GOBIN (or $GOPATH/bin). Puts `velocity` on PATH.
#   make run ARGS=…   Run without building (e.g., `make run ARGS="doctor"`).
#   make test         Run the full test suite.
#   make check        vet + test — what CI should run.
#   make clean        Delete built binaries.

# Stamp the binary with `git describe` so `velocity --version` reports
# something useful. Falls back to "dev" outside a git work tree.
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -ldflags "-X main.version=$(VERSION)"
PKG      := ./cmd/velocity

# Override with `make run ARGS="refresh --since 2026-01"`.
ARGS ?=

.PHONY: all build install run test vet fmt check clean help

all: build

## build: Compile ./velocity into the repo root.
build:
	go build $(LDFLAGS) -o velocity $(PKG)

## install: Install velocity to $GOBIN / $GOPATH/bin so it's on PATH.
install:
	go install $(LDFLAGS) $(PKG)

## run: Run without building. Pass args via ARGS="…".
run:
	go run $(PKG) $(ARGS)

## test: Run the full test suite.
test:
	go test ./...

## vet: go vet — catches common mistakes the compiler doesn't.
vet:
	go vet ./...

## fmt: Format every Go file in-place.
fmt:
	gofmt -w .

## check: vet + test. Run this before pushing.
check: vet test

## clean: Remove the locally built binary.
clean:
	rm -f velocity

## help: Show available targets.
help:
	@grep -E '^##' $(MAKEFILE_LIST) | sed 's/## /  /'
