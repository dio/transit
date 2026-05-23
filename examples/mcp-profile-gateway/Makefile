## examples/mcp-profile-gateway/Makefile - build and test targets for the MCP profile gateway example.

EXAMPLE := mcp-profile-gateway
OUTPUT  := lib$(EXAMPLE).so

ROOT := $(shell git rev-parse --show-toplevel)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

.PHONY: build test e2e run clean download-envoy

build:
	cd $(ROOT)/examples && CGO_ENABLED=1 GOWORK=off go build -trimpath -buildmode=c-shared -o $(EXAMPLE)/$(OUTPUT) ./$(EXAMPLE)/cmd
	@echo "built: $(OUTPUT)"

test:
	cd $(ROOT)/examples && GOWORK=off go test ./$(EXAMPLE) ./$(EXAMPLE)/cmd

e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./$(EXAMPLE)/e2e/... -v -timeout=60s -count=1

run: build
	@echo "Load $(OUTPUT) into Envoy with MCP_PROFILE_GATEWAY_CONFIG; see README.md."

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy
