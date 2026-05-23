ROOT := $(shell git rev-parse --show-toplevel)

ENVOY_GATEWAY_VERSION ?= v1.8.0
K3S_TAG ?= v1.31.6-k3s1
K3D_AGENTS ?= 0

.PHONY: test e2e eg-install clean

test:
	cd $(ROOT)/integrations && go test ./mcp-profile-tiered-router-eg/...

eg-install:
	cd $(ROOT)/integrations && \
	  RUN_MCP_PROFILE_TIERED_ROUTER_EG_INSTALL=1 \
	  ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	  K3S_TAG=$(K3S_TAG) \
	  K3D_AGENTS=$(K3D_AGENTS) \
	  go test -v -count=1 -timeout=10m ./mcp-profile-tiered-router-eg/e2e/

e2e:
	cd $(ROOT)/integrations && \
	  RUN_MCP_PROFILE_TIERED_ROUTER_EG_E2E=1 \
	  ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	  K3S_TAG=$(K3S_TAG) \
	  K3D_AGENTS=$(K3D_AGENTS) \
	  go test -v -count=1 -timeout=25m ./mcp-profile-tiered-router-eg/e2e/

clean:
	@true
