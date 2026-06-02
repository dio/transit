## examples/mcp-streaming-sidecar/Makefile

EXAMPLE := mcp-streaming-sidecar
OUTPUT  := libmcp-streaming-sidecar.so

ROOT     := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

.PHONY: build test clean download-envoy

build:
	cd $(ROOT)/examples/mcp-streaming-sidecar && CGO_ENABLED=1 GOWORK=off go build -trimpath -buildmode=c-shared \
	  -o $(OUTPUT) ./cmd
	@echo "built: $(OUTPUT)"

test:
	cd $(ROOT)/examples/mcp-streaming-sidecar && GOWORK=off go test ./... -count=1

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy
