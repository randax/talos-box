GO ?= go
GOLANGCI_LINT ?= golangci-lint
VERSION ?= $(shell git describe --tags --always)
LDFLAGS := -X github.com/randax/talos-box/internal/version.Version=$(VERSION)

.PHONY: build test e2e lint clean

build:
	mkdir -p ./bin
	$(GO) build -ldflags "$(LDFLAGS)" -o ./bin/tbx ./cmd/tbx
	$(GO) build -ldflags "$(LDFLAGS)" -o ./bin/tbxd ./cmd/tbxd
	$(GO) build -ldflags "$(LDFLAGS)" -o ./bin/tbx-helper ./cmd/tbx-helper

test:
	$(GO) test ./...

e2e:
	@echo "note: e2e tests require a vz-capable Mac"
	$(GO) test -tags e2e ./...

lint:
	$(GOLANGCI_LINT) run

clean:
	rm -rf ./bin
