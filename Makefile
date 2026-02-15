# Copyright 2026 Matheus Pimenta.
# SPDX-License-Identifier: AGPL-3.0

ENVTEST_K8S_VERSION = 1.35.0

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: test build ## Run all build and test targets.

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: tidy
tidy: ## Run go mod tidy.
	go mod tidy

.PHONY: test
test: tidy fmt vet envtest ## Run all unit tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
	GATEWAY_API_CRDS="$(GATEWAY_API_CRDS)" \
	go test ./... -coverprofile cover.out

##@ Build

.PHONY: build
build: fmt vet ## Build the binary.
	CGO_ENABLED=0 go build -o ./bin/cloudflare-gateway-controller .

.PHONY: run
run: fmt vet ## Run the controller locally.
	go run ./main.go --leader-elect=false

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)

## Tool Versions
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')

## Gateway API CRDs from the Go module cache
GATEWAY_API_VERSION ?= $(shell go list -m -f '{{ .Version }}' sigs.k8s.io/gateway-api)
GATEWAY_API_CRDS ?= $(shell go env GOMODCACHE)/sigs.k8s.io/gateway-api@$(GATEWAY_API_VERSION)/config/crd/standard

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary (ideally with version)
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f $(1) ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv "$$(echo "$(1)" | sed "s/-$(3)$$//")" $(1) ;\
}
endef

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
