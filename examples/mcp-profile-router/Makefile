## examples/mcp-profile-router/Makefile - build and test targets for the MCP profile router example.

EXAMPLE := mcp-profile-router
BIN     := dist/$(EXAMPLE)
OUTPUT  := lib$(EXAMPLE).so
COMBO_OUTPUT := lib$(EXAMPLE)-combo.so

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

.PHONY: build test e2e run clean download-envoy

build:
	cd $(ROOT)/examples && go build -trimpath -o $(EXAMPLE)/$(BIN) ./$(EXAMPLE)/cmd
	cd $(ROOT)/examples && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o $(EXAMPLE)/$(OUTPUT) ./$(EXAMPLE)/modules/alacarte
	cd $(ROOT)/examples && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o $(EXAMPLE)/$(COMBO_OUTPUT) ./$(EXAMPLE)/modules/combo
	@echo "built: $(BIN)"
	@echo "built: $(OUTPUT)"
	@echo "built: $(COMBO_OUTPUT)"

test:
	cd $(ROOT)/examples && go test ./$(EXAMPLE) ./$(EXAMPLE)/cmd ./$(EXAMPLE)/modules/alacarte ./$(EXAMPLE)/modules/combo

e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) go test ./$(EXAMPLE)/e2e/... -v -timeout=60s -count=1

run: build
	@echo "Start two backends and the aggregator in separate terminals; see README.md."

clean:
	rm -rf dist $(OUTPUT) $(OUTPUT:.so=.h) $(COMBO_OUTPUT) $(COMBO_OUTPUT:.so=.h)

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy
