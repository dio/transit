ROOT := $(abspath $(CURDIR)/../..)

ENVOY_TAG ?= envoy-0d6e3c60
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
ZIG_ARCH := $(if $(filter amd64,$(ARCH)),x86_64,$(if $(filter arm64,$(ARCH)),aarch64,$(ARCH)))
ZIG_TARGET := $(ZIG_ARCH)-linux-gnu.2.28

.PHONY: test build-module build-pause image pause-image publish eg-install e2e clean

test:
	go test ./... -count=1

build-module:
	$(MAKE) -C $(ROOT) download-zig
	@mkdir -p $(ROOT)/dist
	cd $(ROOT) && TARGET=$(ZIG_TARGET) \
	  CC=$(ROOT)/scripts/zigcc.sh \
	  CGO_ENABLED=1 GOOS=linux GOARCH=$(ARCH) \
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
