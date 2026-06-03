## examples/async-callout/Makefile - build and test targets for the async-callout example.
##
## Run from this directory: make <target>
## Or from the project root: make -C examples/async-callout <target>

EXAMPLE := async-callout
OUTPUT  := libasync-callout.so

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

include $(ROOT)/scripts/zig.mk

.PHONY: build test e2e clean download-envoy

build: $(ZIG_BIN)
	cd $(ROOT)/examples && $(ZIG_BUILD_ENV) go build -trimpath -buildmode=c-shared \
	  -o $(EXAMPLE)/$(OUTPUT) ./$(EXAMPLE)/cmd
	@echo "built: $(OUTPUT)"

test:
	cd $(ROOT)/examples && go test ./$(EXAMPLE)

e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) go test ./$(EXAMPLE)/e2e/... -v -timeout=60s -count=1

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy

$(ENVOY_BIN):
	$(MAKE) -C $(ROOT) download-envoy
