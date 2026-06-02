## examples/tiered-single-envoy/Makefile - build and test targets for the tiered-single-envoy example.
##
## Run from this directory: make <target>
## Or from the project root: make -C examples/tiered-single-envoy <target>

EXAMPLE := tiered-single-envoy

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

SEARCH_PATH := $(ROOT)/examples/cluster-shard-router:$(ROOT)/examples/cluster-router

.PHONY: build test e2e run clean download-envoy

build:
	$(MAKE) -C $(ROOT)/examples/cluster-shard-router build
	$(MAKE) -C $(ROOT)/examples/cluster-router build

test:
	cd $(ROOT)/examples && go vet ./$(EXAMPLE)/...

e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) go test ./$(EXAMPLE)/e2e/... -v -timeout=90s -count=1

run: build $(ENVOY_BIN)
	GODEBUG=cgocheck=0 \
	ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(SEARCH_PATH) \
	$(ENVOY_BIN) -c $(CURDIR)/envoy.yaml --log-level warning

clean:
	$(MAKE) -C $(ROOT)/examples/cluster-shard-router clean
	$(MAKE) -C $(ROOT)/examples/cluster-router clean

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy

$(ENVOY_BIN):
	$(MAKE) -C $(ROOT) download-envoy
