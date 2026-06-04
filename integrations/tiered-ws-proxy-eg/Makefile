ROOT := $(abspath $(CURDIR)/../..)

include $(ROOT)/scripts/zig.mk

_ENVOY_TAG_OVERRIDE   := $(shell grep '^ENVOY_TAG='          $(ROOT)/down/abi_impl/VERSION | cut -d= -f2)
_ENVOY_ASSET_SUFFIX   := $(shell grep '^ENVOY_ASSET_SUFFIX=' $(ROOT)/down/abi_impl/VERSION | cut -d= -f2)

ENVOY_TAG ?= $(if $(_ENVOY_TAG_OVERRIDE),$(_ENVOY_TAG_OVERRIDE),envoy-$(shell grep '^SDK_COMMIT=' $(ROOT)/down/abi_impl/VERSION | cut -d= -f2 | cut -c1-8))
ENVOY_ASSET_SUFFIX ?= $(_ENVOY_ASSET_SUFFIX)

ENVOY_GATEWAY_VERSION ?= v1.8.0
K3S_TAG ?= v1.31.6-k3s1
IMAGE_TAG := $(shell date +%s)
L1_IMAGE ?= transit-tiered-ws-proxy-l1:$(IMAGE_TAG)
L2_IMAGE ?= transit-tiered-ws-proxy-l2:$(IMAGE_TAG)
MOCK_IMAGE ?= transit-tiered-ws-proxy-mock:$(IMAGE_TAG)
PUBLISH_L1_IMAGE ?= ttl.sh/transit-tiered-ws-proxy-l1-$(IMAGE_TAG):1h
PUBLISH_L2_IMAGE ?= ttl.sh/transit-tiered-ws-proxy-l2-$(IMAGE_TAG):1h
PUBLISH_MOCK_IMAGE ?= ttl.sh/transit-tiered-ws-proxy-mock-$(IMAGE_TAG):1h
K3D_IMAGE_IMPORT_MODE ?= direct
K3D_SKIP_IMAGE_IMPORT ?= 0
K3D_AGENTS ?= 0
KEEP_CLUSTER ?= 0
RESET_CLUSTER ?= 1

ARCH := $(shell go env GOARCH)
PLATFORM := linux/$(ARCH)
MOCK_BIN := dist/tiered-ws-proxy-mock.linux-$(ARCH)

.PHONY: test build-modules build-mock l1-image l2-image mock-image publish eg-install e2e clean

test:
	go test ./... -count=1

build-modules: $(ZIG_BIN)
	@mkdir -p $(ROOT)/dist
	cd $(ROOT)/examples && $(ZIG_BUILD_ENV_LINUX) \
	  go build -trimpath -buildmode=c-shared \
	    -o $(ROOT)/dist/libcluster-shard-router.linux-$(ARCH).so ./cluster-shard-router/cmd
	cd $(ROOT)/examples && $(ZIG_BUILD_ENV_LINUX) \
	  go build -trimpath -buildmode=c-shared \
	    -o $(ROOT)/dist/libws-proxy.linux-$(ARCH).so ./ws-proxy/cmd

build-mock:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) \
	  go build -trimpath -o $(MOCK_BIN) ./mock/cmd

l1-image: build-modules
	docker build --platform $(PLATFORM) \
	  --build-arg ENVOY_TAG=$(ENVOY_TAG) \
	  --build-arg ENVOY_ASSET_SUFFIX=$(ENVOY_ASSET_SUFFIX) \
	  --build-arg TARGETARCH=$(ARCH) \
	  -f $(CURDIR)/Dockerfile.l1 \
	  -t $(L1_IMAGE) $(ROOT)

l2-image: build-modules
	docker build --platform $(PLATFORM) \
	  --build-arg ENVOY_TAG=$(ENVOY_TAG) \
	  --build-arg ENVOY_ASSET_SUFFIX=$(ENVOY_ASSET_SUFFIX) \
	  --build-arg TARGETARCH=$(ARCH) \
	  -f $(CURDIR)/Dockerfile.l2 \
	  -t $(L2_IMAGE) $(ROOT)

mock-image: build-mock
	docker build --platform $(PLATFORM) \
	  --build-arg TARGETARCH=$(ARCH) \
	  -f $(CURDIR)/Dockerfile.mock \
	  -t $(MOCK_IMAGE) $(CURDIR)

publish: L1_IMAGE=$(PUBLISH_L1_IMAGE)
publish: L2_IMAGE=$(PUBLISH_L2_IMAGE)
publish: MOCK_IMAGE=$(PUBLISH_MOCK_IMAGE)
publish: l1-image l2-image mock-image
	docker push $(L1_IMAGE)
	docker push $(L2_IMAGE)
	docker push $(MOCK_IMAGE)

eg-install:
	RUN_TIERED_WS_PROXY_EG_INSTALL=1 \
	ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	K3S_TAG=$(K3S_TAG) \
	K3D_AGENTS=$(K3D_AGENTS) \
	KEEP_CLUSTER=$(KEEP_CLUSTER) \
	RESET_CLUSTER=$(RESET_CLUSTER) \
	go test ./e2e/... -run TestEnvoyGatewayInstallOnly -v -timeout=10m -count=1

ifeq ($(SKIP_IMAGE_BUILD),1)
e2e:
else
e2e: l1-image l2-image mock-image
endif
	RUN_TIERED_WS_PROXY_EG_E2E=1 \
	L1_IMAGE=$(L1_IMAGE) \
	L2_IMAGE=$(L2_IMAGE) \
	MOCK_IMAGE=$(MOCK_IMAGE) \
	ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	K3S_TAG=$(K3S_TAG) \
	K3D_AGENTS=$(K3D_AGENTS) \
	K3D_IMAGE_IMPORT_MODE=$(K3D_IMAGE_IMPORT_MODE) \
	K3D_SKIP_IMAGE_IMPORT=$(K3D_SKIP_IMAGE_IMPORT) \
	KEEP_CLUSTER=$(KEEP_CLUSTER) \
	RESET_CLUSTER=$(RESET_CLUSTER) \
	go test ./e2e/... -v -timeout=15m -count=1

clean:
	rm -f $(ROOT)/dist/libcluster-shard-router.linux-$(ARCH).so
	rm -f $(ROOT)/dist/libcluster-shard-router.linux-$(ARCH).h
	rm -f $(ROOT)/dist/libws-proxy.linux-$(ARCH).so
	rm -f $(ROOT)/dist/libws-proxy.linux-$(ARCH).h
	rm -f $(MOCK_BIN)
