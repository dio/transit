## examples/cluster-async-router/Makefile - build and test targets.

EXAMPLE := cluster-async-router
OUTPUT  := libcluster-async-router.so

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

.PHONY: build test e2e e2e-static-tls e2e-static-tls-matches run clean download-envoy

build:
	cd $(ROOT)/examples && CGO_ENABLED=1 GOWORK=off go build -trimpath -buildmode=c-shared \
	  -o $(EXAMPLE)/$(OUTPUT) ./$(EXAMPLE)/cmd
	@echo "built: $(OUTPUT)"

test:
	cd $(ROOT)/examples && GOWORK=off go test ./$(EXAMPLE)

e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./$(EXAMPLE)/e2e/... -v -timeout=60s -count=1

e2e-static-tls: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./$(EXAMPLE)/e2e-static-tls/... -v -timeout=60s -count=1

e2e-static-tls-matches: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./$(EXAMPLE)/e2e-static-tls-matches/... -v -timeout=60s -count=1

run: build $(ENVOY_BIN)
	GODEBUG=cgocheck=0 \
	ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(CURDIR) \
	$(ENVOY_BIN) -c $(CURDIR)/envoy.yaml --log-level warning

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy

$(ENVOY_BIN):
	$(MAKE) -C $(ROOT) download-envoy
