# scripts/zig.mk — shared zig-cc CGO build plumbing.
#
# Include from any Makefile AFTER defining ROOT (absolute path to repo root):
#
#   ROOT := $(abspath $(CURDIR)/../..)
#   include $(ROOT)/scripts/zig.mk
#
# Exports:
#   ZIG_BIN                  – zig binary (auto-downloaded via $(ZIG_BIN) rule)
#   ZIGCC                    – path to scripts/zigcc.sh
#   HOST_TARGET              – zig target triple for the current host
#   ZIG_BUILD_ENV            – env prefix for a CGO go build via zig-cc (host)
#   ZIG_BUILD_ENV_LINUX_AMD64 – env prefix for linux/amd64 cross-build
#   ZIG_BUILD_ENV_LINUX_ARM64 – env prefix for linux/arm64 cross-build
#   ZIG_BUILD_ENV_LINUX      – env prefix for linux/$(ARCH) cross-build (ARCH must be set by caller)
#   build-so                 – recipe macro: $(call build-so, workdir, output, pkg) host build (generic)
#   build-so-linux           – recipe macro: $(call build-so-linux, workdir, output, pkg) linux/$(ARCH) build (generic)
#   build-examples-so        – recipe macro: $(call build-examples-so, output, pkg) host build from $(ROOT)/examples
#   build-examples-so-linux  – recipe macro: $(call build-examples-so-linux, output, pkg) linux/$(ARCH) from $(ROOT)/examples
#   download-zig             – phony target that ensures $(ZIG_BIN) exists

ZIG_VERSION ?= 0.16.0
ZIG_BIN     ?= $(ROOT)/.bin/zig-dist/zig
ZIGCC       := $(ROOT)/scripts/zigcc.sh

GOOS   := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)
_ARCH  := $(shell uname -m | sed 's/arm64/aarch64/')
ZIG_OS := $(if $(filter darwin,$(GOOS)),macos,$(GOOS))

ZIG_URL = https://ziglang.org/download/$(ZIG_VERSION)/zig-$(_ARCH)-$(ZIG_OS)-$(ZIG_VERSION).tar.xz

ifeq ($(GOOS),darwin)
HOST_TARGET = $(_ARCH)-macos
else
HOST_TARGET = $(_ARCH)-linux-gnu.2.28
endif

ZIG_BUILD_ENV             = TARGET=$(HOST_TARGET)           CC=$(ZIGCC) CGO_ENABLED=1
ZIG_BUILD_ENV_LINUX_AMD64 = TARGET=x86_64-linux-gnu.2.28   CC=$(ZIGCC) CGO_ENABLED=1 GOOS=linux GOARCH=amd64
ZIG_BUILD_ENV_LINUX_ARM64 = TARGET=aarch64-linux-gnu.2.28  CC=$(ZIGCC) CGO_ENABLED=1 GOOS=linux GOARCH=arm64

# Dynamic Linux build env – expands $(ARCH) at use time; caller must set ARCH.
_ZIG_ARCH_FOR_LINUX = $(if $(filter amd64,$(ARCH)),x86_64,$(if $(filter arm64,$(ARCH)),aarch64,$(ARCH)))
ZIG_BUILD_ENV_LINUX = TARGET=$(_ZIG_ARCH_FOR_LINUX)-linux-gnu.2.28 CC=$(ZIGCC) CGO_ENABLED=1 GOOS=linux GOARCH=$(ARCH)

# $(call build-so, workdir, output, pkg)
#   Build a c-shared .so for the current host from an arbitrary working directory.
define build-so
cd $(1) && $(ZIG_BUILD_ENV) go build -trimpath -buildmode=c-shared -o $(2) $(3)
endef

# $(call build-so-linux, workdir, output, pkg)
#   Build a c-shared .so for linux/$(ARCH) from an arbitrary working directory. Caller must set ARCH.
define build-so-linux
cd $(1) && $(ZIG_BUILD_ENV_LINUX) go build -trimpath -buildmode=c-shared -o $(2) $(3)
endef

# $(call build-examples-so, output, pkg)
#   Convenience wrapper: host build from $(ROOT)/examples.
define build-examples-so
$(call build-so,$(ROOT)/examples,$(1),$(2))
endef

# $(call build-examples-so-linux, output, pkg)
#   Convenience wrapper: linux/$(ARCH) build from $(ROOT)/examples. Caller must set ARCH.
define build-examples-so-linux
$(call build-so-linux,$(ROOT)/examples,$(1),$(2))
endef

$(ZIG_BIN):
	@mkdir -p $$(dirname $(ZIG_BIN))
	@echo "Downloading zig $(ZIG_VERSION)..."
	@curl -fsSL "$(ZIG_URL)" | tar -xJ --strip-components=1 -C $$(dirname $(ZIG_BIN))

.PHONY: download-zig
download-zig: $(ZIG_BIN)
