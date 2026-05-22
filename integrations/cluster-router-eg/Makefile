ROOT := $(shell git rev-parse --show-toplevel)

ENVOY_TAG ?= envoy-0d6e3c60
ENVOY_GATEWAY_VERSION ?= v1.8.0
K3S_TAG ?= v1.31.6-k3s1
IMAGE_TAG := $(shell date +%s)
IMAGE ?= transit-cluster-router-eg:$(IMAGE_TAG)
CONTROL_PLANE_IMAGE ?= transit-cluster-router-eg-control:$(IMAGE_TAG)
PUBLISH_IMAGE ?= ttl.sh/transit-cluster-router-eg-$(IMAGE_TAG):1h
PUBLISH_CONTROL_PLANE_IMAGE ?= ttl.sh/transit-cluster-router-eg-control-$(IMAGE_TAG):1h
K3D_IMAGE_IMPORT_MODE ?= direct
K3D_AGENTS ?= 0

ARCH := $(shell go env GOARCH)
PLATFORM := linux/$(ARCH)
ZIG_ARCH := $(if $(filter amd64,$(ARCH)),x86_64,$(if $(filter arm64,$(ARCH)),aarch64,$(ARCH)))
ZIG_TARGET := $(ZIG_ARCH)-linux-gnu.2.28
CONTROL_PLANE_BIN := dist/cluster-router-demo.linux-$(ARCH)
CLI_BIN := dist/cluster-router-demo

.PHONY: test build-module image cli control-plane-binary control-plane-image publish eg-install e2e clean

test:
	GOWORK=off go test ./... -count=1

build-module:
	$(MAKE) -C $(ROOT) download-zig
	@mkdir -p $(ROOT)/dist
	cd $(ROOT)/examples && TARGET=$(ZIG_TARGET) \
	  CC=$(ROOT)/scripts/zigcc.sh \
	  CGO_ENABLED=1 GOOS=linux GOARCH=$(ARCH) GOWORK=off \
	  go build -trimpath -buildmode=c-shared \
	    -o $(ROOT)/dist/libcluster-router.linux-$(ARCH).so ./cluster-router/cmd

image: build-module
	docker build --platform $(PLATFORM) \
	  --build-arg ENVOY_TAG=$(ENVOY_TAG) \
	  -f $(CURDIR)/Dockerfile.envoy \
	  -t $(IMAGE) $(ROOT)

cli:
	@mkdir -p dist
	GOWORK=off go build -trimpath -o $(CLI_BIN) ./cmd/demo

control-plane-binary:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) GOWORK=off \
	  go build -trimpath -o $(CONTROL_PLANE_BIN) ./cmd/demo

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
	RUN_CLUSTER_ROUTER_EG_INSTALL=1 \
	ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	K3S_TAG=$(K3S_TAG) \
	K3D_AGENTS=$(K3D_AGENTS) \
	GOWORK=off go test ./e2e/... -run TestEnvoyGatewayInstallOnly -v -timeout=10m -count=1

ifeq ($(SKIP_IMAGE_BUILD),1)
e2e:
else
e2e: image control-plane-image
endif
	RUN_CLUSTER_ROUTER_EG_E2E=1 \
	IMAGE=$(IMAGE) \
	CONTROL_PLANE_IMAGE=$(CONTROL_PLANE_IMAGE) \
	ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	K3S_TAG=$(K3S_TAG) \
	K3D_AGENTS=$(K3D_AGENTS) \
	K3D_IMAGE_IMPORT_MODE=$(K3D_IMAGE_IMPORT_MODE) \
	GOWORK=off go test ./e2e/... -v -timeout=15m -count=1

clean:
	rm -f $(ROOT)/dist/libcluster-router.linux-$(ARCH).so $(ROOT)/dist/libcluster-router.linux-$(ARCH).h
	rm -f $(CONTROL_PLANE_BIN) $(CLI_BIN)
