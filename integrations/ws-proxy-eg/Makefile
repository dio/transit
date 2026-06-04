ROOT := $(abspath $(CURDIR)/../..)

include $(ROOT)/scripts/zig.mk

_ENVOY_TAG_OVERRIDE   := $(shell grep '^ENVOY_TAG='          $(ROOT)/down/abi_impl/VERSION | cut -d= -f2)
_ENVOY_ASSET_SUFFIX   := $(shell grep '^ENVOY_ASSET_SUFFIX=' $(ROOT)/down/abi_impl/VERSION | cut -d= -f2)

ENVOY_TAG ?= $(if $(_ENVOY_TAG_OVERRIDE),$(_ENVOY_TAG_OVERRIDE),envoy-$(shell grep '^SDK_COMMIT=' $(ROOT)/down/abi_impl/VERSION | cut -d= -f2 | cut -c1-8))
ENVOY_ASSET_SUFFIX ?= $(_ENVOY_ASSET_SUFFIX)

ENVOY_GATEWAY_VERSION ?= v1.8.0
K3S_TAG ?= v1.31.6-k3s1
IMAGE_TAG := $(shell date +%s)
IMAGE ?= transit-ws-proxy-eg:$(IMAGE_TAG)
PAUSE_IMAGE ?= transit-ws-proxy-eg-pause:$(IMAGE_TAG)
PUBLISH_IMAGE ?= ttl.sh/transit-ws-proxy-eg-$(IMAGE_TAG):1h
PUBLISH_PAUSE_IMAGE ?= ttl.sh/transit-ws-proxy-eg-pause-$(IMAGE_TAG):1h
K3D_IMAGE_IMPORT_MODE ?= direct
K3D_SKIP_IMAGE_IMPORT ?= 0
K3D_AGENTS ?= 0
KEEP_CLUSTER ?= 0
RESET_CLUSTER ?= 1

ARCH := $(shell go env GOARCH)
PLATFORM := linux/$(ARCH)

.PHONY: test build-module build-pause image pause-image publish eg-install e2e clean

test:
	go test ./... -count=1

build-module: $(ZIG_BIN)
	@mkdir -p $(ROOT)/dist
	cd $(ROOT) && $(ZIG_BUILD_ENV_LINUX) \
	  go build -trimpath -buildmode=c-shared \
	    -o $(ROOT)/dist/libws-proxy-echo.linux-$(ARCH).so ./integrations/ws-proxy-eg/echo/cmd

build-pause:
	@mkdir -p $(ROOT)/dist
	cd $(ROOT) && CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) \
	  go build -trimpath \
	  -o $(ROOT)/dist/pause-linux-$(ARCH) \
	  ./integrations/ws-proxy-eg/pause

image: build-module
	docker build --platform $(PLATFORM) \
	  --build-arg ENVOY_TAG=$(ENVOY_TAG) \
	  --build-arg ENVOY_ASSET_SUFFIX=$(ENVOY_ASSET_SUFFIX) \
	  --build-arg TARGETARCH=$(ARCH) \
	  -f $(CURDIR)/Dockerfile.envoy \
	  -t $(IMAGE) $(ROOT)

pause-image: build-pause
	docker build --platform $(PLATFORM) \
	  --build-arg TARGETARCH=$(ARCH) \
	  -f $(CURDIR)/Dockerfile.pause \
	  -t $(PAUSE_IMAGE) $(ROOT)

publish: IMAGE=$(PUBLISH_IMAGE)
publish: PAUSE_IMAGE=$(PUBLISH_PAUSE_IMAGE)
publish: image pause-image
	docker push $(IMAGE)
	docker push $(PAUSE_IMAGE)

eg-install:
	RUN_WS_PROXY_EG_INSTALL=1 \
	ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	K3S_TAG=$(K3S_TAG) \
	K3D_AGENTS=$(K3D_AGENTS) \
	KEEP_CLUSTER=$(KEEP_CLUSTER) \
	RESET_CLUSTER=$(RESET_CLUSTER) \
	go test ./e2e/... -run TestEnvoyGatewayInstallOnly -v -timeout=10m -count=1

ifeq ($(SKIP_IMAGE_BUILD),1)
e2e:
else
e2e: image pause-image
endif
	RUN_WS_PROXY_EG_E2E=1 \
	IMAGE=$(IMAGE) \
	PAUSE_IMAGE=$(PAUSE_IMAGE) \
	ENVOY_GATEWAY_VERSION=$(ENVOY_GATEWAY_VERSION) \
	K3S_TAG=$(K3S_TAG) \
	K3D_AGENTS=$(K3D_AGENTS) \
	K3D_IMAGE_IMPORT_MODE=$(K3D_IMAGE_IMPORT_MODE) \
	K3D_SKIP_IMAGE_IMPORT=$(K3D_SKIP_IMAGE_IMPORT) \
	KEEP_CLUSTER=$(KEEP_CLUSTER) \
	RESET_CLUSTER=$(RESET_CLUSTER) \
	go test ./e2e/... -v -timeout=15m -count=1

clean:
	rm -f $(ROOT)/dist/libws-proxy-echo.linux-$(ARCH).so
	rm -f $(ROOT)/dist/libws-proxy-echo.linux-$(ARCH).h
	rm -f $(ROOT)/dist/pause-linux-$(ARCH)
