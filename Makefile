GO_TOOL := GOWORK=off go tool -modfile=$(CURDIR)/tools/go.mod
GOLANGCI_CONFIG := $(CURDIR)/.golangci.yml

ZIG_VERSION ?= 0.16.0
ZIG_BIN ?= $(CURDIR)/.bin/zig-dist/zig

ENVOY_BIN ?= $(CURDIR)/.bin/envoy

EXAMPLE     ?= hello
EXAMPLE_CMD ?= ./examples/$(EXAMPLE)/cmd
ENVOY_YAML  ?= $(CURDIR)/examples/$(EXAMPLE)/envoy.yaml

GOOS   := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

_ARCH  := $(shell uname -m | sed 's/arm64/aarch64/')
ZIG_OS := $(if $(filter darwin,$(GOOS)),macos,$(GOOS))

# Zig download URL (deferred so ZIG_VERSION overrides take effect).
ZIG_URL = https://ziglang.org/download/$(ZIG_VERSION)/zig-$(_ARCH)-$(ZIG_OS)-$(ZIG_VERSION).tar.xz

# Envoy: raw binaries from dio/envoy-builder releases.
#
# ENVOY_TAG: read from VERSION if set; otherwise derived as envoy-{SDK_COMMIT[:8]}.
# ENVOY_ASSET_SUFFIX: appended to the binary asset name (e.g. -auto-host-sni).
# ABI_SOURCE: "gomod" (default) or "release" (download abi.h from release asset).
_ENVOY_TAG_OVERRIDE   := $(shell grep '^ENVOY_TAG='          down/abi_impl/VERSION | cut -d= -f2)
_ENVOY_ASSET_SUFFIX   := $(shell grep '^ENVOY_ASSET_SUFFIX=' down/abi_impl/VERSION | cut -d= -f2)
_ABI_SOURCE           := $(shell grep '^ABI_SOURCE='         down/abi_impl/VERSION | cut -d= -f2)

ENVOY_TAG  := $(if $(_ENVOY_TAG_OVERRIDE),$(_ENVOY_TAG_OVERRIDE),envoy-$(shell grep '^SDK_COMMIT=' down/abi_impl/VERSION | cut -d= -f2 | cut -c1-8))
ABI_SOURCE := $(if $(_ABI_SOURCE),$(_ABI_SOURCE),gomod)

ENVOY_URL = https://github.com/dio/envoy-builder/releases/download/$(ENVOY_TAG)/envoy-$(GOOS)-$(GOARCH)$(_ENVOY_ASSET_SUFFIX)
ABI_URL   = https://github.com/dio/envoy-builder/releases/download/$(ENVOY_TAG)/abi.h

# Host target triple for zig cc.
ifeq ($(GOOS),darwin)
HOST_TARGET = $(_ARCH)-macos
else
HOST_TARGET = $(_ARCH)-linux-gnu.2.28
endif

.PHONY: all
all: build

# Download zig on demand.
$(ZIG_BIN):
	@mkdir -p $$(dirname $(ZIG_BIN))
	@echo "Downloading zig $(ZIG_VERSION)..."
	@curl -fsSL "$(ZIG_URL)" | tar -xJ --strip-components=1 -C $$(dirname $(ZIG_BIN))

# Download Envoy on demand.
$(ENVOY_BIN):
	@mkdir -p $$(dirname $(ENVOY_BIN))
	@echo "Downloading Envoy $(ENVOY_TAG) ($(GOOS)-$(GOARCH))..."
	@curl -fsSL -L "$(ENVOY_URL)" -o $(ENVOY_BIN)
	@chmod +x $(ENVOY_BIN)

.PHONY: download-zig
download-zig: $(ZIG_BIN)

.PHONY: download-envoy
download-envoy: $(ENVOY_BIN)

.PHONY: build
build: $(ZIG_BIN)
	@mkdir -p dist .bin
	cd .bin && TARGET=$(HOST_TARGET) \
	CC=$(CURDIR)/scripts/zigcc.sh \
	CGO_ENABLED=1 \
	go build -trimpath -buildmode=c-shared \
		-o $(CURDIR)/dist/lib$(EXAMPLE).so $(CURDIR)/$(EXAMPLE_CMD)

.PHONY: build-linux-amd64
build-linux-amd64: $(ZIG_BIN)
	@mkdir -p dist .bin
	cd .bin && TARGET=x86_64-linux-gnu.2.28 \
	CC=$(CURDIR)/scripts/zigcc.sh \
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
	go build -trimpath -buildmode=c-shared \
		-o $(CURDIR)/dist/lib$(EXAMPLE).linux-amd64.so $(CURDIR)/$(EXAMPLE_CMD)

.PHONY: build-linux-arm64
build-linux-arm64: $(ZIG_BIN)
	@mkdir -p dist .bin
	cd .bin && TARGET=aarch64-linux-gnu.2.28 \
	CC=$(CURDIR)/scripts/zigcc.sh \
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 \
	go build -trimpath -buildmode=c-shared \
		-o $(CURDIR)/dist/lib$(EXAMPLE).linux-arm64.so $(CURDIR)/$(EXAMPLE_CMD)

.PHONY: run
run: build $(ENVOY_BIN)
	GODEBUG=cgocheck=0 \
	ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(CURDIR)/dist \
	$(ENVOY_BIN) -c $(ENVOY_YAML) --log-level warning

.PHONY: test
test:
	go test -race ./...

.PHONY: e2e
e2e: $(ENVOY_BIN)
	$(MAKE) -C e2e ENVOY_BIN=$(ENVOY_BIN) test

.PHONY: vet
vet:
	go vet ./...
	cd examples && go vet ./...
	cd e2e && go vet ./...
	cd integrations && go vet ./...

.PHONY: format
format:
	$(GO_TOOL) golangci-lint fmt --config $(GOLANGCI_CONFIG)
	cd examples && $(GO_TOOL) golangci-lint fmt --config $(GOLANGCI_CONFIG)
	cd e2e && $(GO_TOOL) golangci-lint fmt --config $(GOLANGCI_CONFIG)
	cd integrations && $(GO_TOOL) golangci-lint fmt --config $(GOLANGCI_CONFIG)

.PHONY: format-check
format-check:
	$(GO_TOOL) golangci-lint fmt --config $(GOLANGCI_CONFIG) --diff .
	cd examples && $(GO_TOOL) golangci-lint fmt --config $(GOLANGCI_CONFIG) --diff .
	cd e2e && $(GO_TOOL) golangci-lint fmt --config $(GOLANGCI_CONFIG) --diff .
	cd integrations && $(GO_TOOL) golangci-lint fmt --config $(GOLANGCI_CONFIG) --diff .

.PHONY: lint
lint:
	$(GO_TOOL) golangci-lint run --config $(GOLANGCI_CONFIG) --timeout 5m
	cd examples && $(GO_TOOL) golangci-lint run --config $(GOLANGCI_CONFIG) --timeout 5m
	cd e2e && $(GO_TOOL) golangci-lint run --config $(GOLANGCI_CONFIG) --timeout 5m
	cd integrations && $(GO_TOOL) golangci-lint run --config $(GOLANGCI_CONFIG) --timeout 5m

.PHONY: tidy
tidy:
	go mod tidy
	cd e2e && go mod tidy
	cd examples && go mod tidy
	cd integrations && go mod tidy
	cd tools && GOWORK=off go mod tidy
	go work sync

# sync-abi downloads abi.h from the GitHub release pinned by ENVOY_TAG in
# down/abi_impl/VERSION. Use this when ABI_SOURCE=release (patched builds).
.PHONY: sync-abi
sync-abi:
	@echo "Downloading abi.h from $(ENVOY_TAG)..."
	@chmod u+w down/abi_impl/abi.h 2>/dev/null || true
	@curl -fsSL -L "$(ABI_URL)" -o down/abi_impl/abi.h
	@echo "abi.h synced from $(ABI_URL)"

# update-sdk upgrades the Envoy dynamic modules SDK to the given Envoy commit and
# syncs down/abi_impl/abi.h + down/abi_impl/VERSION in one step.
# Usage: make update-sdk ENVOY_COMMIT=<full-or-short-commit>
# For patched builds set ABI_SOURCE=release in VERSION and use sync-abi instead.
.PHONY: update-sdk
update-sdk:
	@if [ -z "$(ENVOY_COMMIT)" ]; then \
		echo "Usage: make update-sdk ENVOY_COMMIT=<commit>"; \
		exit 1; \
	fi
	GOWORK=off go get github.com/envoyproxy/envoy/source/extensions/dynamic_modules@$(ENVOY_COMMIT)
	GOWORK=off go mod tidy
	@NEW_VER=$$(grep 'envoyproxy/envoy/source/extensions/dynamic_modules ' go.mod | awk '{print $$2}'); \
	NEW_COMMIT=$$(echo "$$NEW_VER" | sed 's/.*-//'); \
	gsed -i "s|^SDK_VERSION=.*|SDK_VERSION=$$NEW_VER|" down/abi_impl/VERSION; \
	gsed -i "s|^SDK_COMMIT=.*|SDK_COMMIT=$$NEW_COMMIT|" down/abi_impl/VERSION; \
	echo "SDK updated to $$NEW_VER"; \
	if [ "$(ABI_SOURCE)" = "release" ]; then \
		$(MAKE) sync-abi; \
	else \
		MODCACHE=$$(go env GOPATH)/pkg/mod; \
		ABI_SRC="$$MODCACHE/github.com/envoyproxy/envoy/source/extensions/dynamic_modules@$$NEW_VER/abi/abi.h"; \
		chmod u+w down/abi_impl/abi.h; \
		cp "$$ABI_SRC" down/abi_impl/abi.h; \
		echo "abi.h copied from module cache"; \
	fi
	$(MAKE) tidy

# check-abi verifies abi.h provenance matches VERSION.
# When ABI_SOURCE=release the gomod version check is skipped (patched abi.h is a
# superset of the upstream module version and intentionally differs).
.PHONY: check-abi
check-abi:
	@if [ "$(ABI_SOURCE)" = "release" ]; then \
		echo "ABI_SOURCE=release: abi.h sourced from $(ENVOY_TAG) release (skipping gomod version check)"; \
	else \
		gomod_ver=$$(grep 'envoyproxy/envoy/source/extensions/dynamic_modules ' go.mod | awk '{print $$2}'); \
		abi_ver=$$(grep '^SDK_VERSION=' down/abi_impl/VERSION | cut -d= -f2); \
		if [ "$$gomod_ver" != "$$abi_ver" ]; then \
			echo "ABI DRIFT: go.mod has $$gomod_ver but down/abi_impl/VERSION records $$abi_ver"; \
			echo "Run: make update-sdk ENVOY_COMMIT=<commit>"; \
			exit 1; \
		fi; \
		echo "abi.h OK: $$gomod_ver"; \
	fi

.PHONY: clean
clean:
	rm -rf dist/*.so dist/*.h .bin/*.o
