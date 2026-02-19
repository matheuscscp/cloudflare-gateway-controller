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

# Allows for defining additional Go test args, e.g. '-tags integration'.
GO_TEST_ARGS ?=

# Docker image name for local builds.
IMG ?= cloudflare-gateway-controller:dev

.PHONY: all
all: test build build-cfgwctl ## Run all build and test targets.

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate RBAC manifests and wire them into the Helm chart.
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths="./..." output:rbac:artifacts:config=config/rbac
	./hack/gen-chart-rbac.sh

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
	go test ./... $(GO_TEST_ARGS) -coverprofile cover.out

##@ Build

.PHONY: build
build: fmt vet ## Build the binary.
	CGO_ENABLED=0 go build -o ./bin/cloudflare-gateway-controller .

.PHONY: build-cfgwctl
build-cfgwctl: fmt vet ## Build the cfgwctl CLI binary.
	CGO_ENABLED=0 go build -o ./bin/cfgwctl ./cmd/cfgwctl

.PHONY: docker-build
docker-build: ## Build the controller Docker image locally.
	docker buildx build -t $(IMG) --load .

.PHONY: test-e2e
test-e2e: docker-build build-cfgwctl ## Run end-to-end tests against a kind cluster.
	hack/e2e-test.sh 2>&1 | stdbuf -oL tee test-e2e.log

.PHONY: run
run: fmt vet ## Run the controller locally against the current kubeconfig cluster.
	go run ./main.go --leader-elect=false 2>&1 | stdbuf -oL tee run.log

.PHONY: run-debug
run-debug: fmt vet ## Run the controller locally with debug logging (verbosity level 1).
	go run ./main.go --leader-elect=false --zap-log-level=debug 2>&1 | stdbuf -oL tee run.log

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_GEN_VERSION)
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)

## Tool Versions
CONTROLLER_GEN_VERSION ?= v0.19.0
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_GEN_VERSION))

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
