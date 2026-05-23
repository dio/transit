REPO_ROOT := $(abspath ..)

ENVOY_BIN ?= $(REPO_ROOT)/.bin/envoy
GO_TEST_ARGS ?= ./...
RUN ?=
TIMEOUT ?= 30s
COUNT ?= 1

.PHONY: test
test: $(ENVOY_BIN)
	ENVOY_BIN=$(ENVOY_BIN) go test $(GO_TEST_ARGS) -v -timeout=$(TIMEOUT) -count=$(COUNT) $(if $(RUN),-run '$(RUN)')

$(ENVOY_BIN):
	$(MAKE) -C $(REPO_ROOT) download-envoy
