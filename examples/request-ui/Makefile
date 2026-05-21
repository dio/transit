## examples/request-ui/Makefile — build and test targets for the request-ui example.
##
## Run from this directory: make <target>
## Or from the project root: make -C examples/request-ui <target>

OUTPUT := librequest-ui.so

ROOT      := $(shell git rev-parse --show-toplevel)
ENVOY_BIN ?= $(ROOT)/.bin/envoy
GOARCH    ?= $(shell go env GOARCH)

.PHONY: build test e2e simulate run clean download-envoy

## build: compile librequest-ui.so for the current host
build:
	cd $(ROOT) && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
	  -o examples/request-ui/$(OUTPUT) ./examples/request-ui/cmd
	@echo "built: $(OUTPUT)"

## test: run unit tests (no Envoy required)
test:
	cd $(ROOT) && go test ./examples/request-ui/...

## e2e: build .so, start testserver + Envoy, run Node.js e2e tests
e2e: build
	ENVOY_BIN=$(ENVOY_BIN) bash $(dir $(lastword $(MAKEFILE_LIST)))e2e/run.sh

## simulate: zero-dependency demo (no Envoy, no Postgres)
##           generates synthetic traffic; UI at http://localhost:6062/
simulate:
	cd $(ROOT) && REQUI_ADDR=0.0.0.0:6062 go run ./examples/request-ui/cmd/simulate

## run: build, then start Envoy with the request-ui filter
##      UI at http://localhost:6062/ ; proxy at http://localhost:10000/
run: build $(ENVOY_BIN)
	REQUI_ADDR=0.0.0.0:6062 \
	GODEBUG=cgocheck=0 \
	ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(dir $(abspath $(OUTPUT))) \
	$(ENVOY_BIN) -c $(CURDIR)/envoy.yaml --log-level warning

## clean: remove compiled .so and header files
clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

## download-envoy: delegate to root Makefile
download-envoy:
	$(MAKE) -C $(ROOT) download-envoy
