## examples/spa/Makefile — build and test targets for the SPA example.
##
## Run from this directory: make <target>
## Or from the project root: make -C examples/spa <target>

OUTPUT := libspa.so

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

.PHONY: build test e2e e2e-js clean download-envoy

## build: compile libspa.so for the current host
build:
	cd $(ROOT)/examples && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
	  -o spa/$(OUTPUT) ./spa/cmd
	@echo "built: $(OUTPUT)"

## test: run unit tests (no Envoy required)
test:
	cd $(ROOT)/examples && go test ./spa/...

## e2e: build .so and run Go HTTP-level e2e tests (no browser required)
e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) go test ./spa/e2e/... -v -timeout=60s -count=1

## e2e-js: build .so, start Envoy, run Lightpanda browser tests (requires Node 24)
e2e-js: build
	ENVOY_BIN=$(ENVOY_BIN) bash $(dir $(lastword $(MAKEFILE_LIST)))e2e/run.sh

## clean: remove compiled .so and header files
clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

## download-envoy: delegate to root Makefile
download-envoy:
	$(MAKE) -C $(ROOT) download-envoy

$(ENVOY_BIN):
	$(MAKE) -C $(ROOT) download-envoy
