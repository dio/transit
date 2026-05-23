## examples/request-ui/Makefile — build and test targets for the request-ui example.
##
## Run from this directory: make <target>
## Or from the project root: make -C examples/request-ui <target>

EXAMPLE := request-ui
OUTPUT  := librequest-ui.so

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy
GOARCH    ?= $(shell go env GOARCH)

.PHONY: build test e2e simulate run clean download-envoy

## build: compile librequest-ui.so for the current host
build:
	cd $(ROOT)/examples && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
	  -o $(EXAMPLE)/$(OUTPUT) ./$(EXAMPLE)/cmd
	@echo "built: $(OUTPUT)"

## test: run unit tests (no Envoy required)
test:
	cd $(ROOT)/examples && go test ./$(EXAMPLE)/...

## e2e: build .so + testserver, start Envoy, run Go e2e tests
e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) go test ./$(EXAMPLE)/e2e/... -v -timeout=120s -count=1

## simulate: zero-dependency demo (no Envoy, no Postgres)
##           generates synthetic traffic; UI at http://localhost:6062/
simulate:
	cd $(ROOT) && REQUI_ADDR=0.0.0.0:6062 go run ./examples/$(EXAMPLE)/cmd/simulate

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

$(ENVOY_BIN):
	$(MAKE) -C $(ROOT) download-envoy
