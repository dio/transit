GO_TOOL          := GOWORK=off go tool -modfile=tools/go.mod
EXAMPLES_GO_TOOL := GOWORK=off go tool -modfile=$(CURDIR)/tools/go.mod

ZIG_VERSION   ?= 0.16.0
ZIG_BIN       ?= $(CURDIR)/.bin/zig-dist/zig

ENVOY_VERSION ?= 1.38.0
ENVOY_BIN     ?= $(CURDIR)/.bin/envoy

EXAMPLE     ?= hello
EXAMPLE_CMD ?= ./examples/$(EXAMPLE)/cmd
ENVOY_YAML  ?= $(CURDIR)/examples/$(EXAMPLE)/envoy.yaml

GOOS   := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

_ARCH  := $(shell uname -m | sed 's/arm64/aarch64/')
ZIG_OS := $(if $(filter darwin,$(GOOS)),macos,$(GOOS))

# Deferred so overriding ZIG_VERSION / ENVOY_VERSION on the command line takes effect.
ZIG_URL   = https://ziglang.org/download/$(ZIG_VERSION)/zig-$(_ARCH)-$(ZIG_OS)-$(ZIG_VERSION).tar.xz
ENVOY_URL = https://archive.tetratelabs.io/envoy/download/v$(ENVOY_VERSION)/envoy-v$(ENVOY_VERSION)-$(GOOS)-$(GOARCH).tar.xz

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
	@echo "Downloading Envoy $(ENVOY_VERSION) for $(GOOS)-$(GOARCH)..."
	@curl -fsSL "$(ENVOY_URL)" | tar -xJ --strip-components=2 -C $$(dirname $(ENVOY_BIN))
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
	CGO_ENABLED=1 GOWORK=off go build -trimpath -buildmode=c-shared \
		-o $(CURDIR)/dist/lib$(EXAMPLE).so $(CURDIR)/$(EXAMPLE_CMD)

.PHONY: build-linux-amd64
build-linux-amd64: $(ZIG_BIN)
	@mkdir -p dist .bin
	cd .bin && TARGET=x86_64-linux-gnu.2.28 \
	CC=$(CURDIR)/scripts/zigcc.sh \
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 GOWORK=off \
	go build -trimpath -buildmode=c-shared \
		-o $(CURDIR)/dist/lib$(EXAMPLE).linux-amd64.so $(CURDIR)/$(EXAMPLE_CMD)

.PHONY: build-linux-arm64
build-linux-arm64: $(ZIG_BIN)
	@mkdir -p dist .bin
	cd .bin && TARGET=aarch64-linux-gnu.2.28 \
	CC=$(CURDIR)/scripts/zigcc.sh \
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 GOWORK=off \
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
	cd e2e && ENVOY_BIN=$(ENVOY_BIN) go test ./... -v -timeout=90s

.PHONY: e2e-hello
e2e-hello: $(ENVOY_BIN)
	cd examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./hello/e2e/... -v -timeout=60s

.PHONY: e2e-sse-tap
e2e-sse-tap: $(ENVOY_BIN)
	cd examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./sse-tap/e2e/... -v -timeout=60s

.PHONY: e2e-request-ui
e2e-request-ui: $(ENVOY_BIN)
	cd examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./request-ui/e2e/... -v -timeout=120s

.PHONY: e2e-lb-policy
e2e-lb-policy: $(ENVOY_BIN)
	cd examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./lb-policy/e2e/... -v -timeout=60s

.PHONY: vet
vet:
	go vet ./...

.PHONY: format
format:
	$(GO_TOOL) golangci-lint fmt
	cd examples && $(EXAMPLES_GO_TOOL) golangci-lint fmt

.PHONY: format-check
format-check:
	$(GO_TOOL) golangci-lint fmt --diff .
	cd examples && $(EXAMPLES_GO_TOOL) golangci-lint fmt --diff .

.PHONY: lint
lint:
	$(GO_TOOL) golangci-lint run --timeout 5m
	cd examples && $(EXAMPLES_GO_TOOL) golangci-lint run --timeout 5m

.PHONY: tidy
tidy:
	go mod tidy
	cd e2e && GOWORK=off go mod tidy
	cd examples && GOWORK=off go mod tidy
	cd tools && GOWORK=off go mod tidy

# update-sdk upgrades the Envoy dynamic modules SDK to the given Envoy commit and
# syncs down/abi_impl/abi.h + down/abi_impl/VERSION in one step.
# Usage: make update-sdk ENVOY_COMMIT=<full-or-short-commit>
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
	MODCACHE=$$(go env GOPATH)/pkg/mod; \
	ABI_SRC="$$MODCACHE/github.com/envoyproxy/envoy/source/extensions/dynamic_modules@$$NEW_VER/abi/abi.h"; \
	chmod u+w down/abi_impl/abi.h; \
	cp "$$ABI_SRC" down/abi_impl/abi.h; \
	sed -i.bak "s|^SDK_VERSION=.*|SDK_VERSION=$$NEW_VER|" down/abi_impl/VERSION; \
	sed -i.bak "s|^SDK_COMMIT=.*|SDK_COMMIT=$$NEW_COMMIT|" down/abi_impl/VERSION; \
	rm -f down/abi_impl/VERSION.bak; \
	echo "SDK updated to $$NEW_VER"

# check-abi verifies that the vendored abi.h (down/abi_impl/abi.h) was taken from
# the same SDK version that go.mod depends on. Run this after `go get` updates.
.PHONY: check-abi
check-abi:
	@gomod_ver=$$(grep 'envoyproxy/envoy/source/extensions/dynamic_modules ' go.mod | awk '{print $$2}'); \
	abi_ver=$$(grep '^SDK_VERSION=' down/abi_impl/VERSION | cut -d= -f2); \
	if [ "$$gomod_ver" != "$$abi_ver" ]; then \
		echo "ABI DRIFT: go.mod has $$gomod_ver but down/abi_impl/VERSION records $$abi_ver"; \
		echo "Update abi.h and down/abi_impl/VERSION to match go.mod (see VERSION for instructions)."; \
		exit 1; \
	fi; \
	echo "abi.h OK: $$gomod_ver"

.PHONY: clean
clean:
	rm -rf dist/*.so dist/*.h .bin/*.o
