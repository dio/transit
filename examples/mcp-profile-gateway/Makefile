## examples/mcp-profile-gateway/Makefile - build and test targets for the MCP profile gateway example.

EXAMPLE := mcp-profile-gateway
OUTPUT  := lib$(EXAMPLE).so

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

include $(ROOT)/scripts/zig.mk

.PHONY: build test e2e run clean download-envoy

build: $(ZIG_BIN)
	cd $(ROOT)/examples && $(ZIG_BUILD_ENV) go build -trimpath -buildmode=c-shared -o $(EXAMPLE)/$(OUTPUT) ./$(EXAMPLE)/cmd
	@echo "built: $(OUTPUT)"

test:
	cd $(ROOT)/examples && go test ./$(EXAMPLE) ./$(EXAMPLE)/cmd

e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) go test ./$(EXAMPLE)/e2e/... -v -timeout=60s -count=1

run: build
	@echo "Load $(OUTPUT) into Envoy with MCP_PROFILE_GATEWAY_CONFIG; see README.md."

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy
