GO ?= go
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS := -X main.commit=$(COMMIT)

.PHONY: build run test vet install

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o chuanshu ./cmd/chuanshu

# One command from source to running: compile, then launch the TUI
# (the daemon auto-starts in the background on first use).
run: build
	./chuanshu

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

install:
	sh scripts/install.sh
