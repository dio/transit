## examples/orange/Makefile - build and run targets for the orange LLM proxy.
##
## Run from this directory: make <target>
## Or from the project root: make -C examples/orange <target>

EXAMPLE := orange
OUTPUT  := liborange.so

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

# TLS trust bundle for upstream HTTPS (auto_san_validation needs a root CA).
# Override with `make run ORANGE_TRUSTED_CA=/path/to/bundle.pem`.
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
ORANGE_TRUSTED_CA ?= /etc/ssl/cert.pem
else
ORANGE_TRUSTED_CA ?= $(firstword $(wildcard /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem))
endif

.PHONY: build test e2e run demo clean download-envoy

build:
	cd $(ROOT)/examples && CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
	  -o $(EXAMPLE)/$(OUTPUT) ./$(EXAMPLE)/cmd
	@echo "built: $(OUTPUT)"

test:
	cd $(ROOT)/examples && go test ./$(EXAMPLE)/...

e2e: build
	cd $(ROOT)/examples && ENVOY_BIN=$(ENVOY_BIN) go test ./$(EXAMPLE)/e2e/... -v -timeout=90s -count=1

run: build $(ENVOY_BIN) $(CURDIR)/envoy.yaml
	@if [ -z "$$OPENAI_API_KEY" ]; then echo "OPENAI_API_KEY is unset (env://OPENAI_API_KEY in orange.yaml will fail Envoy boot)"; exit 1; fi
	@if [ -z "$$ANTHROPIC_API_KEY" ]; then echo "ANTHROPIC_API_KEY is unset (env://ANTHROPIC_API_KEY in orange.yaml will fail Envoy boot)"; exit 1; fi
	GODEBUG=cgocheck=0 \
	ORANGE_CONFIG=$(CURDIR)/orange.yaml \
	ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(CURDIR) \
	$(ENVOY_BIN) -c $(CURDIR)/envoy.yaml --log-level info

# `make demo` is `make run` with a pre-flight banner that prints the curls
# you're likely to want. Aimed at first-time readers; existing users can keep
# using `make run`.
demo:
	@echo "============================================================"
	@echo " orange — multi-provider LLM proxy demo"
	@echo "============================================================"
	@echo " Listener  : http://127.0.0.1:8080"
	@echo " Admin     : http://127.0.0.1:9901"
	@echo ""
	@echo " Try (from another shell):"
	@echo ""
	@echo "   # OpenAI (gpt-4o-mini → routed to api.openai.com)"
	@echo "   curl -s :8080/v1/chat/completions -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"gpt-4o-mini\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
	@echo ""
	@echo "   # Anthropic (claude-3-5-haiku-latest → routed to api.anthropic.com)"
	@echo "   curl -s :8080/v1/messages -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"claude-3-5-haiku-latest\",\"max_tokens\":32,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
	@echo ""
	@echo "   # Streaming (SSE): add -N to curl and \"stream\": true to the body."
	@echo ""
	@echo "   # Per-host TLS counters (both should tick after the curls above):"
	@echo "   curl -s :9901/clusters | grep orange_default:: | grep cx_total"
	@echo "============================================================"
	@$(MAKE) run

.PHONY: $(CURDIR)/envoy.yaml
$(CURDIR)/envoy.yaml: $(CURDIR)/envoy.tmpl.yaml
	@if [ -z "$(ORANGE_TRUSTED_CA)" ] || [ ! -f "$(ORANGE_TRUSTED_CA)" ]; then \
	  echo "ORANGE_TRUSTED_CA=$(ORANGE_TRUSTED_CA) is not a readable file; set ORANGE_TRUSTED_CA to your TLS trust bundle"; \
	  exit 1; \
	fi
	ORANGE_TRUSTED_CA=$(ORANGE_TRUSTED_CA) envsubst '$$ORANGE_TRUSTED_CA' < $< > $@

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h) $(CURDIR)/envoy.yaml

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy

$(ENVOY_BIN):
	$(MAKE) -C $(ROOT) download-envoy
