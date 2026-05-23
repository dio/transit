ROOT := $(shell git rev-parse --show-toplevel)

ENVOY_TAG ?= envoy-0d6e3c60
ENVOY_GATEWAY_VERSION ?= v1.8.0
K3S_TAG ?= v1.31.6-k3s1
IMAGE_TAG := $(shell date +%s)
IMAGE ?= transit-mcp-profile-tiered-router-eg:$(IMAGE_TAG)
CONTROL_PLANE_IMAGE ?= transit-mcp-profile-tiered-router-eg-demo:$(IMAGE_TAG)
PUBLISH_IMAGE ?= ttl.sh/transit-mcp-profile-tiered-router-eg-$(IMAGE_TAG):1h
PUBLISH_CONTROL_PLANE_IMAGE ?= ttl.sh/transit-mcp-profile-tiered-router-eg-demo-$(IMAGE_TAG):1h
K3D_IMAGE_IMPORT_MODE ?= direct
K3D_SKIP_IMAGE_IMPORT ?= 0
K3D_AGENTS ?= 0
KEEP_CLUSTER ?= 0
RESET_CLUSTER ?= 1

ARCH := $(shell go env GOARCH)
PLATFORM := linux/$(ARCH)
ZIG_ARCH := $(if $(filter amd64,$(ARCH)),x86_64,$(if $(filter arm64,$(ARCH)),aarch64,$(ARCH)))
ZIG_TARGET := $(ZIG_ARCH)-linux-gnu.2.28
CONTROL_PLANE_BIN := dist/mcp-profile-tiered-router-demo.linux-$(ARCH)

.PHONY: test build-modules image control-plane-binary control-plane-image publish e2e eg-install clean

test:
	cd $(ROOT)/integrations && go test ./mcp-profile-tiered-router-eg/... -count=1

build-modules:
	$(MAKE) -C $(ROOT) download-zig
	@mkdir -p $(ROOT)/dist
	cd $(ROOT)/examples && TARGET=$(ZIG_TARGET) \
	  CC=$(ROOT)/scripts/zigcc.sh \
	  CGO_ENABLED=1 GOOS=linux GOARCH=$(ARCH) \
	  go build -trimpath -buildmode=c-shared \
	    -o $(ROOT)/dist/libmcp-profile-gateway.linux-$(ARCH).so ./mcp-profile-gateway/cmd
	cd $(ROOT)/examples && TARGET=$(ZIG_TARGET) \
	  CC=$(ROOT)/scripts/zigcc.sh \
	  CGO_ENABLED=1 GOOS=linux GOARCH=$(ARCH) \
	  go build -trimpath -buildmode=c-shared \
	    -o $(ROOT)/dist/libmcp-catalog-router.linux-$(ARCH).so ./mcp-catalog-router/cmd
	cd $(ROOT)/examples && TARGET=$(ZIG_TARGET) \
	  CC=$(ROOT)/scripts/zigcc.sh \
	  CGO_ENABLED=1 GOOS=linux GOARCH=$(ARCH) \
	  go build -trimpath -buildmode=c-shared \
	    -o $(ROOT)/dist/libcluster-router.linux-$(ARCH).so ./cluster-router/cmd

image: build-modules
	docker build --platform $(PLATFORM) \
	  --build-arg ENVOY_TAG=$(ENVOY_TAG) \
	  -f $(CURDIR)/Dockerfile.envoy \
	  -t $(IMAGE) $(ROOT)

demo-binary: control-plane-binary

control-plane-binary:
	@mkdir -p dist
	cd $(ROOT)/integrations && CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) \
	  go build -trimpath -o $(CURDIR)/$(CONTROL_PLANE_BIN) ./mcp-profile-tiered-router-eg/cmd/demo

demo-image: control-plane-image

control-plane-image: control-plane-binary
	docker build --platform $(PLATFORM) \
	  --build-arg TARGETARCH=$(ARCH) \
	  -f Dockerfile.demo \
	  -t $(CONTROL_PLANE_IMAGE) .

publish: IMAGE=$(PUBLISH_IMAGE)
publish: CONTROL_PLANE_IMAGE=$(PUBLISH_CONTROL_PLANE_IMAGE)
publish: image control-plane-image
	docker push $(IMAGE)
	docker push $(CONTROL_PLANE_IMAGE)

eg-install:
	cd $(ROOT)/integrations && \
	  RUN_MCP_PROFILE_TIERED_ROUTER_EG_INSTALL=1 \
	  ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	  K3S_TAG=$(K3S_TAG) \
	  K3D_AGENTS=$(K3D_AGENTS) \
	  KEEP_CLUSTER=$(KEEP_CLUSTER) \
	  RESET_CLUSTER=$(RESET_CLUSTER) \
	  \
	  go test -v -count=1 -timeout=10m ./mcp-profile-tiered-router-eg/e2e/

ifeq ($(SKIP_IMAGE_BUILD),1)
e2e:
else
e2e: image control-plane-image
endif
	cd $(ROOT)/integrations && \
	  RUN_MCP_PROFILE_TIERED_ROUTER_EG_E2E=1 \
	  IMAGE=$(IMAGE) \
	  DEMO_IMAGE=$(CONTROL_PLANE_IMAGE) \
	  ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	  K3S_TAG=$(K3S_TAG) \
	  K3D_AGENTS=$(K3D_AGENTS) \
	  K3D_IMAGE_IMPORT_MODE=$(K3D_IMAGE_IMPORT_MODE) \
	  K3D_SKIP_IMAGE_IMPORT=$(K3D_SKIP_IMAGE_IMPORT) \
	  KEEP_CLUSTER=$(KEEP_CLUSTER) \
	  RESET_CLUSTER=$(RESET_CLUSTER) \
	  \
	  go test -v -count=1 -timeout=25m ./mcp-profile-tiered-router-eg/e2e/

clean:
	rm -f $(ROOT)/dist/libmcp-profile-gateway.linux-$(ARCH).so $(ROOT)/dist/libmcp-profile-gateway.linux-$(ARCH).h
	rm -f $(ROOT)/dist/libmcp-catalog-router.linux-$(ARCH).so $(ROOT)/dist/libmcp-catalog-router.linux-$(ARCH).h
	rm -f $(ROOT)/dist/libcluster-router.linux-$(ARCH).so $(ROOT)/dist/libcluster-router.linux-$(ARCH).h
	rm -f $(CONTROL_PLANE_BIN)
