## examples/llm-decider/Makefile - build and test targets for the llm-decider example.
##
## Run from this directory: make <target>
## Or from the project root: make -C examples/llm-decider <target>

EXAMPLE := llm-decider
OUTPUT  := libllm-decider.so

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy
INTEGRATION := $(ROOT)/integrations/llm-decider-eg

include $(ROOT)/scripts/zig.mk

.PHONY: build test e2e e2e-eg clean download-envoy

build: $(ZIG_BIN)
	cd $(ROOT)/examples && $(ZIG_BUILD_ENV) go build -trimpath -buildmode=c-shared \
	  -o $(EXAMPLE)/$(OUTPUT) ./$(EXAMPLE)/cmd
	@echo "built: $(OUTPUT)"

test:
	cd $(ROOT)/examples && go test ./$(EXAMPLE)/... -count=1

e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) go test ./$(EXAMPLE)/e2e/... -v -timeout=60s -count=1

# The same module under Envoy Gateway, registered through the gateway control
# plane rather than a hand-written bootstrap.
e2e-eg:
	@if [ -f $(INTEGRATION)/Makefile ]; then \
	  $(MAKE) -C $(INTEGRATION) e2e; \
	else \
	  echo "integrations/llm-decider-eg is not present yet"; \
	  exit 1; \
	fi

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy

$(ENVOY_BIN):
	$(MAKE) -C $(ROOT) download-envoy
