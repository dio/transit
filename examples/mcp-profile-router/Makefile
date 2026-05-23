## examples/mcp-profile-router/Makefile - build and test targets for the MCP profile router example.

EXAMPLE := mcp-profile-router
BIN     := dist/$(EXAMPLE)
OUTPUT  := lib$(EXAMPLE).so
COMBO_OUTPUT := lib$(EXAMPLE)-combo.so

ROOT := $(shell git rev-parse --show-toplevel)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

.PHONY: build test e2e run clean download-envoy

build:
	cd $(ROOT)/examples && GOWORK=off go build -trimpath -o $(EXAMPLE)/$(BIN) ./$(EXAMPLE)/cmd
	cd $(ROOT)/examples && CGO_ENABLED=1 GOWORK=off go build -trimpath -buildmode=c-shared -o $(EXAMPLE)/$(OUTPUT) ./$(EXAMPLE)/module
	cd $(ROOT)/examples && CGO_ENABLED=1 GOWORK=off go build -trimpath -buildmode=c-shared -o $(EXAMPLE)/$(COMBO_OUTPUT) ./$(EXAMPLE)/combo
	@echo "built: $(BIN)"
	@echo "built: $(OUTPUT)"
	@echo "built: $(COMBO_OUTPUT)"

test:
	cd $(ROOT)/examples && GOWORK=off go test ./$(EXAMPLE)

e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./$(EXAMPLE)/e2e/... -v -timeout=60s -count=1

run: build
	@echo "Start two backends and the aggregator in separate terminals; see README.md."

clean:
	rm -rf dist $(OUTPUT) $(OUTPUT:.so=.h) $(COMBO_OUTPUT) $(COMBO_OUTPUT:.so=.h)

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy
