## examples/spa/Makefile — build and test targets for the SPA example.
##
## Run from this directory: make <target>
## Or from the project root: make -C examples/spa <target>

OUTPUT := libspa.so

ROOT      := $(shell git rev-parse --show-toplevel)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

.PHONY: build test e2e clean download-envoy

## build: compile libspa.so for the current host
build:
	cd $(ROOT) && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
	  -o examples/spa/$(OUTPUT) ./examples/spa/cmd
	@echo "built: $(OUTPUT)"

## test: run unit tests (no Envoy required)
test:
	cd $(ROOT) && go test ./examples/spa/...

## e2e: build .so, start Envoy, run Lightpanda browser tests
e2e: build
	ENVOY_BIN=$(ENVOY_BIN) bash $(dir $(lastword $(MAKEFILE_LIST)))e2e/run.sh

## clean: remove compiled .so and header files
clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

## download-envoy: delegate to root Makefile
download-envoy:
	$(MAKE) -C $(ROOT) download-envoy
