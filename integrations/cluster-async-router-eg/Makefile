## integrations/cluster-async-router-eg/Makefile - P2.
##
## Build/test the body-driven routing path on a local k3d cluster. P3 adds the
## per-host TLS upstreams on top of this without changing the Make targets.

ROOT := $(abspath $(CURDIR)/../..)

ENVOY_TAG ?= envoy-0d6e3c60
ENVOY_GATEWAY_VERSION ?= v1.8.0
K3S_TAG ?= v1.31.6-k3s1
IMAGE_TAG := $(shell date +%s)
IMAGE ?= transit-cluster-async-router-eg:$(IMAGE_TAG)
DEMO_IMAGE ?= transit-cluster-async-router-eg-demo:$(IMAGE_TAG)
PUBLISH_IMAGE ?= ttl.sh/transit-cluster-async-router-eg-$(IMAGE_TAG):1h
PUBLISH_DEMO_IMAGE ?= ttl.sh/transit-cluster-async-router-eg-demo-$(IMAGE_TAG):1h
K3D_IMAGE_IMPORT_MODE ?= direct
K3D_SKIP_IMAGE_IMPORT ?= 0
K3D_AGENTS ?= 0
KEEP_CLUSTER ?= 0
RESET_CLUSTER ?= 1

ARCH := $(shell go env GOARCH)
PLATFORM := linux/$(ARCH)
ZIG_ARCH := $(if $(filter amd64,$(ARCH)),x86_64,$(if $(filter arm64,$(ARCH)),aarch64,$(ARCH)))
ZIG_TARGET := $(ZIG_ARCH)-linux-gnu.2.28
DEMO_BIN := dist/cluster-async-router-demo.linux-$(ARCH)
CLI_BIN := dist/cluster-async-router-demo

.PHONY: test build-module image cli demo-binary demo-image publish eg-install e2e clean

test:
	go test ./... -count=1

build-module:
	$(MAKE) -C $(ROOT) download-zig
	@mkdir -p $(ROOT)/dist
	cd $(ROOT)/examples && TARGET=$(ZIG_TARGET) \
	  CC=$(ROOT)/scripts/zigcc.sh \
	  CGO_ENABLED=1 GOOS=linux GOARCH=$(ARCH) \
	  go build -trimpath -buildmode=c-shared \
	    -o $(ROOT)/dist/libcluster-async-router.linux-$(ARCH).so ./cluster-async-router/cmd

image: build-module
	docker build --platform $(PLATFORM) \
	  --build-arg ENVOY_TAG=$(ENVOY_TAG) \
	  -f $(CURDIR)/Dockerfile.envoy \
	  -t $(IMAGE) $(ROOT)

cli:
	@mkdir -p dist
	go build -trimpath -o $(CLI_BIN) ./cmd/demo

demo-binary:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) \
	  go build -trimpath -o $(DEMO_BIN) ./cmd/demo

demo-image: demo-binary
	docker build --platform $(PLATFORM) \
	  --build-arg TARGETARCH=$(ARCH) \
	  -f Dockerfile.demo \
	  -t $(DEMO_IMAGE) .

publish: IMAGE=$(PUBLISH_IMAGE)
publish: DEMO_IMAGE=$(PUBLISH_DEMO_IMAGE)
publish: image demo-image
	docker push $(IMAGE)
	docker push $(DEMO_IMAGE)

eg-install:
	RUN_CLUSTER_ASYNC_ROUTER_EG_INSTALL=1 \
	ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	K3S_TAG=$(K3S_TAG) \
	K3D_AGENTS=$(K3D_AGENTS) \
	KEEP_CLUSTER=$(KEEP_CLUSTER) \
	RESET_CLUSTER=$(RESET_CLUSTER) \
	go test ./e2e/... -run TestEnvoyGatewayInstallOnly -v -timeout=10m -count=1

ifeq ($(TLS_VIA_SDS),1)
e2e: TLS_VIA_SDS_FLAG = TLS_VIA_SDS=1
else
e2e: TLS_VIA_SDS_FLAG =
endif

ifeq ($(SKIP_IMAGE_BUILD),1)
e2e:
else
e2e: image demo-image
endif
	RUN_CLUSTER_ASYNC_ROUTER_EG_E2E=1 \
	IMAGE=$(IMAGE) \
	DEMO_IMAGE=$(DEMO_IMAGE) \
	ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	K3S_TAG=$(K3S_TAG) \
	K3D_AGENTS=$(K3D_AGENTS) \
	K3D_IMAGE_IMPORT_MODE=$(K3D_IMAGE_IMPORT_MODE) \
	K3D_SKIP_IMAGE_IMPORT=$(K3D_SKIP_IMAGE_IMPORT) \
	KEEP_CLUSTER=$(KEEP_CLUSTER) \
	RESET_CLUSTER=$(RESET_CLUSTER) \
	$(TLS_VIA_SDS_FLAG) \
	go test ./e2e/... -v -timeout=15m -count=1

clean:
	rm -f $(ROOT)/dist/libcluster-async-router.linux-$(ARCH).so $(ROOT)/dist/libcluster-async-router.linux-$(ARCH).h
	rm -f $(DEMO_BIN) $(CLI_BIN)
