## examples/mcp-streaming-sidecar/Makefile

EXAMPLE := mcp-streaming-sidecar
OUTPUT  := libmcp-streaming-sidecar.so

ROOT     := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

include $(ROOT)/scripts/zig.mk

.PHONY: build test clean download-envoy

build: $(ZIG_BIN)
	cd $(ROOT)/examples/mcp-streaming-sidecar && $(ZIG_BUILD_ENV) GOWORK=off go build -trimpath -buildmode=c-shared \
	  -o $(OUTPUT) ./cmd
	@echo "built: $(OUTPUT)"

test:
	cd $(ROOT)/examples/mcp-streaming-sidecar && GOWORK=off go test ./... -count=1

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy
