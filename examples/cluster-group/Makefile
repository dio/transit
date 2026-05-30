## examples/cluster-group/Makefile - build and test targets for the cluster-group example.
##
## Run from this directory: make <target>
## Or from the project root: make -C examples/cluster-group <target>

EXAMPLE := cluster-group
OUTPUT  := libcluster-group.so

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

.PHONY: build test e2e clean download-envoy

build:
	cd $(ROOT)/examples && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
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
