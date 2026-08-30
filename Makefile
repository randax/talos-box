GO ?= go
GOLANGCI_LINT ?= golangci-lint
UNAME_S ?= $(shell uname -s)
VERSION ?= $(shell git describe --tags --always)
LDFLAGS := -X github.com/randax/talos-box/internal/version.Version=$(VERSION)
_e2e_test := $(GO) test -tags e2e -count=1 -timeout 90m ./...

.PHONY: build binaries sign test e2e e2e-all lint clean

build: sign

binaries:
	mkdir -p ./bin
	$(GO) build -ldflags "$(LDFLAGS)" -o ./bin/tbx ./cmd/tbx
	$(GO) build -ldflags "$(LDFLAGS)" -o ./bin/tbxd ./cmd/tbxd
	$(GO) build -ldflags "$(LDFLAGS)" -o ./bin/tbx-helper ./cmd/tbx-helper

sign: binaries
	codesign --force --sign - --entitlements ./build/entitlements.plist ./bin/tbxd
	codesign --force --sign - --entitlements ./build/entitlements.plist ./bin/tbx

test:
	$(GO) test ./...
	bash scripts/ci/test_make_e2e_contract.sh

ifeq ($(UNAME_S),Darwin)
e2e e2e-all: build
else
e2e e2e-all: binaries
endif

e2e:
	@echo "note: e2e tests require a supported VZ or QEMU hypervisor"
	$(if $(TBX_E2E_HYPERVISOR),TBX_E2E_HYPERVISOR=$(TBX_E2E_HYPERVISOR) )$(_e2e_test)

e2e-all:
	TBX_E2E_HYPERVISOR=vz $(_e2e_test)
	TBX_E2E_HYPERVISOR=qemu $(_e2e_test)

lint:
	$(GOLANGCI_LINT) run

clean:
	rm -rf ./bin
