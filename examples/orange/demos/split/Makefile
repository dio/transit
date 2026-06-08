# Traffic split demo: 34/33/33 weight distribution across three identical endpoints
# This demo shows traffic splitting for load balancing and canary testing.

ROOT := $(abspath ../../../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
ORANGE_TRUSTED_CA ?= /etc/ssl/cert.pem
else
ORANGE_TRUSTED_CA ?= $(firstword $(wildcard /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem))
endif

.PHONY: demo run

demo: | banner run

banner:
	@echo "=========================================="
	@echo " Traffic Split Demo"
	@echo "=========================================="
	@echo ""
	@echo " This demo shows traffic splitting/canary:"
	@echo " Requests are split 34/33/33 across three Vertex AI Anthropic endpoints"
	@echo " (requires GCP_PROJECT and GCP_SERVICE_ACCOUNT_JSON set)"
	@echo ""
	@echo " Use the split key for routing:"
	@echo "   ORANGE_API_KEY=demo/dio/sk-split"
	@echo ""
	@echo " Try (run multiple times to see different endpoints):"
	@echo "   curl -s localhost:8080/v1/messages \\"
	@echo "     -H 'Authorization: Bearer demo/dio/sk-split' \\"
	@echo "     -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"claude-haiku-4-5\",\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
	@echo ""

run: $(ENVOY_BIN)
	@if [ -z "$(ORANGE_TRUSTED_CA)" ] || [ ! -f "$(ORANGE_TRUSTED_CA)" ]; then \
	  echo "ORANGE_TRUSTED_CA=$(ORANGE_TRUSTED_CA) is not a readable file; set ORANGE_TRUSTED_CA to your TLS trust bundle"; \
	  exit 1; \
	fi
	@ENVOY_YAML=$$(mktemp /tmp/orange-split-envoy.XXXXXX.yaml) ; \
	 trap "rm -f $$ENVOY_YAML" EXIT INT TERM ; \
	 ORANGE_TRUSTED_CA=$(ORANGE_TRUSTED_CA) envsubst '$$ORANGE_TRUSTED_CA' \
	   < $(CURDIR)/envoy.tmpl.yaml > $$ENVOY_YAML ; \
	 cd $(ROOT)/examples/orange && \
	 GITHUB_TOKEN=$${GITHUB_TOKEN:-$$(gh auth token 2>/dev/null)} \
	 GODEBUG=cgocheck=0 \
	 ORANGE_CONFIG=$(CURDIR)/orange.yaml \
	 ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(ROOT)/examples/orange \
	 $(ENVOY_BIN) -c $$ENVOY_YAML --log-level info
