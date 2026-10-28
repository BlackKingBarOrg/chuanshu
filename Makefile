GO ?= go

.PHONY: build test vet install

build:
	$(GO) build -o chuanshu ./cmd/chuanshu

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

install:
	sh scripts/install.sh
