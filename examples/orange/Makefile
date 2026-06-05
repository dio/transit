## examples/orange/Makefile - build and run targets for the orange LLM proxy.
##
## Run from this directory: make <target>
## Or from the project root: make -C examples/orange <target>

EXAMPLE := orange
OUTPUT  := liborange.so

ROOT := $(abspath $(CURDIR)/../..)
ENVOY_BIN ?= $(ROOT)/.bin/envoy

include $(ROOT)/scripts/zig.mk

# Load .env if present (e.g. API keys). Variables in .env use KEY=value format
# and override nothing already set on the command line.
-include $(CURDIR)/.env
export ANTHROPIC_API_KEY OPENAI_API_KEY GEMINI_API_KEY GCP_PROJECT GCP_SERVICE_ACCOUNT_JSON AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN GITHUB_TOKEN ORANGE_MCP_SESSION_KEYS

# Config file: override with `make run ORANGE_CONFIG=/path/to/orange.yaml` or
# set ORANGE_CONFIG in .env. Defaults to orange.yaml in this directory.
ORANGE_CONFIG ?= $(CURDIR)/orange.yaml

# TLS trust bundle for upstream HTTPS (auto_san_validation needs a root CA).
# Override with `make run ORANGE_TRUSTED_CA=/path/to/bundle.pem`.
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
ORANGE_TRUSTED_CA ?= /etc/ssl/cert.pem
else
ORANGE_TRUSTED_CA ?= $(firstword $(wildcard /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem))
endif

.PHONY: build build-linux-amd64 build-linux-arm64 test e2e run demo clean download-envoy

build: $(ZIG_BIN)
	cd $(ROOT)/examples && $(ZIG_BUILD_ENV) \
	  GOWORK=off go build -trimpath -buildmode=c-shared \
	    -o $(CURDIR)/$(OUTPUT) ./$(EXAMPLE)/cmd
	@echo "built: $(OUTPUT)"

build-linux-amd64: $(ZIG_BIN)
	cd $(ROOT)/examples && $(ZIG_BUILD_ENV_LINUX_AMD64) \
	  GOWORK=off go build -trimpath -buildmode=c-shared \
	    -o $(CURDIR)/$(OUTPUT:.so=.linux-amd64.so) ./$(EXAMPLE)/cmd
	@echo "built: $(OUTPUT:.so=.linux-amd64.so)"

build-linux-arm64: $(ZIG_BIN)
	cd $(ROOT)/examples && $(ZIG_BUILD_ENV_LINUX_ARM64) \
	  GOWORK=off go build -trimpath -buildmode=c-shared \
	    -o $(CURDIR)/$(OUTPUT:.so=.linux-arm64.so) ./$(EXAMPLE)/cmd
	@echo "built: $(OUTPUT:.so=.linux-arm64.so)"

test:
	cd $(ROOT)/examples && GOWORK=off go test ./$(EXAMPLE)/...

e2e: build
	cd $(ROOT)/examples && \
	  GITHUB_TOKEN=$${GITHUB_TOKEN:-$$(gh auth token 2>/dev/null)} \
	  ENVOY_BIN=$(ENVOY_BIN) GOWORK=off go test ./$(EXAMPLE)/e2e/... -v -timeout=90s -count=1

run: build $(ENVOY_BIN) $(CURDIR)/envoy.yaml
	@for var in OPENAI_API_KEY ANTHROPIC_API_KEY GEMINI_API_KEY GITHUB_TOKEN; do \
	  if grep -q "env://$$var" "$(ORANGE_CONFIG)" 2>/dev/null && [ -z "$$(eval echo \$$$$var)" ]; then \
	    echo "$$var is unset but referenced via env://$$var in $(ORANGE_CONFIG)"; exit 1; fi; done
	GODEBUG=cgocheck=0 \
	ORANGE_CONFIG=$(ORANGE_CONFIG) \
	ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(CURDIR) \
	$(ENVOY_BIN) -c $(CURDIR)/envoy.yaml --log-level info

# `make demo` is `make run` with a pre-flight banner that prints the curls
# you're likely to want. Aimed at first-time readers; existing users can keep
# using `make run`.
demo:
	@echo "============================================================"
	@echo " orange — multi-provider LLM proxy demo"
	@echo "============================================================"
	@echo " Listener  : http://localhost:8080"
	@echo " Admin     : http://localhost:9901"
	@echo ""
	@echo " Try (from another shell):"
	@echo ""
	@echo "   # OpenAI Chat Completions (gpt-4o-mini → api.openai.com)"
	@echo "   curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"gpt-4o-mini\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
	@echo ""
	@echo "   # OpenAI Responses API — non-streaming"
	@echo "   curl -s localhost:8080/v1/responses -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"gpt-4o-mini\",\"input\":\"hi\"}'"
	@echo ""
	@echo "   # OpenAI Chat Completions — streaming (SSE)"
	@echo "   curl -N -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"gpt-4o-mini\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"count 1 to 5\"}]}'"
	@echo ""
	@echo "   # OpenAI Responses API — streaming (SSE)"
	@echo "   curl -N -s localhost:8080/v1/responses -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"gpt-4o-mini\",\"stream\":true,\"input\":\"count 1 to 5\"}'"
	@echo ""
	@echo "   # Anthropic Messages (claude-haiku-4-5 → api.anthropic.com)"
	@echo "   curl -s localhost:8080/v1/messages -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"claude-haiku-4-5\",\"max_tokens\":32,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
	@echo ""
	@echo "   # Anthropic Messages — streaming (SSE)"
	@echo "   curl -N -s localhost:8080/v1/messages -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"claude-haiku-4-5\",\"max_tokens\":64,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"count 1 to 5\"}]}'"
	@echo ""
	@echo "   # Codex CLI through orange (uses Responses API path):"
	@echo "   cd $(CURDIR) && ./codex-demo"
	@echo ""
	@echo "   # Codex CLI through orange — WebSocket mode:"
	@echo "   cd $(CURDIR) && ./codex-demo --ws"
	@echo ""
	@echo "   # Codex CLI through orange — one-shot prompt:"
	@echo "   cd $(CURDIR) && ./codex-demo exec \\"
	@echo "     'write a hello-world HTTP server in Go'"
	@echo ""
	@echo "   # Codex CLI through orange — one-shot prompt over WebSocket:"
	@echo "   cd $(CURDIR) && ./codex-demo --ws exec \\"
	@echo "     'write a hello-world HTTP server in Go'"
	@echo ""
	@echo "   # Validate Codex is using orange's local model catalog:"
	@echo "   cd $(CURDIR) && ORANGE_CODEX_HOME=\"\$$(mktemp -d)\" ./codex-demo debug models \\"
	@echo "     | jq '.models[] | select(.slug == \"gpt-4o-mini\") | {slug, context_window, max_context_window, supports_parallel_tool_calls}'"
	@echo ""
	@echo "   # Validate Codex over WebSocket with Codex-side WS trace logs:"
	@echo "   cd $(CURDIR) && ORANGE_CODEX_HOME=\"\$$(mktemp -d)\" ORANGE_CODEX_TRACE=1 \\"
	@echo "     ./codex-demo --ws exec --json 'reply with exactly: orange responsesws ok'"
	@echo ""
	@echo "   # AWS Bedrock (amazon.nova-lite-v1:0 → bedrock-runtime.us-east-1.amazonaws.com)"
	@echo "   curl -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"amazon.nova-lite-v1:0\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'"
	@echo ""
	@echo "   # AWS Bedrock — streaming"
	@echo "   curl -N -s localhost:8080/v1/chat/completions -H 'content-type: application/json' \\"
	@echo "     -d '{\"model\":\"amazon.nova-lite-v1:0\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"count 1 to 5\"}]}'"
	@echo ""
	@echo "   # OpenAI-compatible model list served by orange:"
	@echo "   curl -s localhost:8080/v1/models \\"
	@echo "     | jq '.data[] | select(.id == \"gpt-4o-mini\")'"
	@echo ""
	@echo "   # Token usage counters (should tick after each request):"
	@echo "   curl -s localhost:9901/stats | grep orange_"
	@echo ""
	@echo "   # Per-host TLS counters:"
	@echo "   curl -s localhost:9901/clusters | grep orange_default:: | grep cx_total"
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
	rm -f $(OUTPUT) $(OUTPUT:.so=.h) \
	  $(OUTPUT:.so=.linux-amd64.so) $(OUTPUT:.so=.linux-amd64.h) \
	  $(OUTPUT:.so=.linux-arm64.so) $(OUTPUT:.so=.linux-arm64.h) \
	  $(CURDIR)/envoy.yaml

download-envoy:
	$(MAKE) -C $(ROOT) download-envoy

$(ENVOY_BIN):
	$(MAKE) -C $(ROOT) download-envoy
