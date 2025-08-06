# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= 4.14.0

# You can use podman or docker as a container engine. Notice that there are some options that might be only valid for one of them.
ENGINE ?= docker

# OPERATOR_SDK_VERSION defines the operator-sdk version to download from GitHub releases.
OPERATOR_SDK_VERSION ?= 1.28.0

# YQ_VERSION defines the yq version to download from GitHub releases.
YQ_VERSION ?= v4.45.4

# OPM_VERSION defines the opm version to download from GitHub releases.
OPM_VERSION ?= v1.52.0

# Konflux catalog configuration
PACKAGE_NAME_KONFLUX = lifecycle-agent
CATALOG_TEMPLATE_KONFLUX = .konflux/catalog/catalog-template.in.yaml
CATALOG_KONFLUX = .konflux/catalog/$(PACKAGE_NAME_KONFLUX)/catalog.yaml

# Konflux bundle image configuration
BUNDLE_NAME_SUFFIX = operator-bundle-4-14
PRODUCTION_BUNDLE_NAME = operator-bundle

# By default we build the same architecture we are running
# Override this by specifying a different GOARCH in your environment
HOST_ARCH ?= $(shell uname -m)

# Convert from uname format to GOARCH format
ifeq ($(HOST_ARCH),aarch64)
	HOST_ARCH=arm64
endif
ifeq ($(HOST_ARCH),x86_64)
	HOST_ARCH=amd64
endif

# Define GOARCH as HOST_ARCH if not otherwise defined
ifndef GOARCH
	GOARCH=$(HOST_ARCH)
endif

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# This is a requirement for 'setup-envtest.sh' in the test target.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
export PATH  := $(PATH):$(PWD)/bin
GOFLAGS := -mod=mod
SHELL = /usr/bin/env GOFLAGS=$(GOFLAGS) bash -o pipefail

.SHELLFLAGS = -ec

# CHANNELS define the bundle channels used in the bundle.
# Add a new line here if you would like to change its default config. (E.g CHANNELS = "preview,fast,stable")
# To re-generate a bundle for other specific channels without changing the standard setup, you can:
# - use the CHANNELS as arg of the bundle target (e.g make bundle CHANNELS=preview,fast,stable)
# - use environment variables to overwrite this value (e.g export CHANNELS="preview,fast,stable")
ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif

# DEFAULT_CHANNEL defines the default channel used in the bundle.
# Add a new line here if you would like to change its default config. (E.g DEFAULT_CHANNEL = "stable")
# To re-generate a bundle for any other default channel without changing the default setup, you can:
# - use the DEFAULT_CHANNEL as arg of the bundle target (e.g make bundle DEFAULT_CHANNEL=stable)
# - use environment variables to overwrite this value (e.g export DEFAULT_CHANNEL="stable")
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)

# USE_IMAGE_DIGESTS defines if images are resolved via tags or digests
# You can enable this value if you would like to use SHA Based Digests
# To enable set flag to true
USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

# IMAGE_TAG_BASE defines the docker.io namespace and part of the image name for remote images.
# This variable is used to construct full image tags for bundle and catalog images.
#
# For example, running 'make bundle-build bundle-push catalog-build catalog-push' will build and push both
# openshift.io/lifecycle-agent-bundle:$VERSION and openshift.io/lifecycle-agent-catalog:$VERSION.
IMAGE_NAME ?= lifecycle-agent-operator
IMAGE_TAG_BASE ?= quay.io/openshift-kni/$(IMAGE_NAME)

# BUNDLE_IMG defines the image:tag used for the bundle.
# You can use it as an arg. (E.g make bundle-build BUNDLE_IMG=<some-registry>/<project-name-bundle>:<tag>)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:$(VERSION)

# Image URL to use all building/pushing image targets
IMG ?= $(IMAGE_TAG_BASE):$(VERSION)

CONTROLLER_GEN = $(shell pwd)/bin/controller-gen
GOTESTSUM = $(shell pwd)/bin/gotestsum
KUSTOMIZE = $(shell pwd)/bin/kustomize
MOCK_GEN = $(shell pwd)/bin/mockgen
OPERATOR_SDK = $(shell which operator-sdk 2>/dev/null || echo "$(shell pwd)/bin/operator-sdk")
OPM = $(shell which opm 2>/dev/null || echo "$(shell pwd)/bin/opm")
YQ = $(shell which yq 2>/dev/null || echo "$(shell pwd)/bin/yq")

# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "crd"

default: help

test:
	@echo "Stub test target"

manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) $(CRD_OPTIONS) rbac:roleName=manager-role webhook paths="./..." output:crd:artifacts:config=config/crd/bases

generate: controller-gen mock-gen # generate-code
    ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."
	PATH="${PROJECT_DIR}/bin:${PATH}" go generate $(shell go list ./...)

generate-code: ## Generate code containing Clientset, Informers, Listers
	@echo "Running generate-code"
	hack/update-codegen.sh

controller-gen: ## Download controller-gen locally if necessary.
	$(call go-get-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen@v0.13.0)

mock-gen: ## Download mockgen locally if necessary.
	$(call go-get-tool,$(MOCK_GEN),go.uber.org/mock/mockgen@v0.3.0)

.PHONY: fmt
fmt: ## Run go fmt against code.
	@echo "Running go fmt"
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	@echo "Running go vet"
	go vet ./...

.PHONY: unittest
unittest:
	@echo "Running unit tests"
	go test -v ./...

.PHONY: common-deps-update
common-deps-update:	controller-gen kustomize
	go mod tidy

.PHONY: scorecard
scorecard: operator-sdk ## Run scorecard tests against bundle
ifeq ($(KUBECONFIG),)
	@echo "Running scorecard tests requires KUBECONFIG set to access cluster"
else
	@echo "Running scorecard"
	$(OPERATOR_SDK) scorecard bundle
endif

.PHONY: install-go-test-coverage
install-go-test-coverage:
	go install github.com/vladopajic/go-test-coverage/v2@latest

.PHONY: check-coverage
check-coverage: install-go-test-coverage
	go test ./... -coverprofile=./cover.out -covermode=atomic -coverpkg=./...
	${GOBIN}/go-test-coverage --config=./.testcoverage.yml

.PHONY: ci-job
ci-job: common-deps-update generate fmt vet golangci-lint unittest shellcheck bashate yamllint bundle-check

kustomize: ## Download kustomize locally if necessary.
	$(call go-get-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5@v5.1.1)

# go-get-tool will 'go get' any package $2 and install it to $1.
PROJECT_DIR := $(shell dirname $(abspath $(firstword $(MAKEFILE_LIST))))
define go-get-tool
@[ -f $(1) ] || { \
set -e ;\
TMP_DIR=$$(mktemp -d) ;\
cd $$TMP_DIR ;\
go mod init tmp ;\
echo "Downloading $(2)" ;\
GOBIN=$(PROJECT_DIR)/bin GOFLAGS=-mod=mod go install $(2) ;\
go mod tidy ;\
rm -rf $$TMP_DIR ;\
}
endef

##@ Build

build: generate fmt vet ## Build manager binary.
	go build -o bin/manager main/main.go

run: manifests generate fmt vet ## Run a controller from your host.
	PRECACHE_WORKLOAD_IMG=${IMG} go run ./main/main.go

debug: manifests generate fmt vet ## Run a controller from your host that accepts remote attachment.
	PRECACHE_WORKLOAD_IMG=${IMG} dlv debug --headless --listen 127.0.0.1:2345 --api-version 2 --accept-multiclient ./main.go

docker-build: ## Build container image with the manager.
	${ENGINE} build --arch ${GOARCH} --build-arg GOARCH=${GOARCH} -t ${IMG} -f Dockerfile .

docker-push: docker-build ## Push container image with the manager.
	${ENGINE} push ${IMG}

##@ Deployment

install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl delete -f -

deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG) && PRECACHE_WORKLOAD_IMG=$(IMG) envsubst < related-images/in.yaml > related-images/patch.yaml
	$(KUSTOMIZE) build config/default | kubectl apply -f -

undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/default | kubectl delete -f -

.PHONY: bundle
bundle: operator-sdk manifests kustomize ## Generate bundle manifests and metadata, then validate generated files.
	$(OPERATOR_SDK) generate kustomize manifests -q
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG) && PRECACHE_WORKLOAD_IMG=$(IMG) envsubst < related-images/in.yaml > related-images/patch.yaml
	$(KUSTOMIZE) build config/manifests | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	$(OPERATOR_SDK) bundle validate ./bundle
	if [ "$$(uname)" = "Darwin" ]; then \
		sed -i '' '/^[[:space:]]*createdAt:/d' bundle/manifests/lifecycle-agent.clusterserviceversion.yaml; \
	else \
		sed -i '/^[[:space:]]*createdAt:/d' bundle/manifests/lifecycle-agent.clusterserviceversion.yaml; \
	fi

.PHONY: bundle-build
bundle-build: bundle docker-push ## Build the bundle image.
	${ENGINE} build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: bundle-build ## Push the bundle image.
	${ENGINE} push $(BUNDLE_IMG)

.PHONY: bundle-check
bundle-check: bundle
	hack/check-git-tree.sh

.PHONY: bundle-run
bundle-run: # Install bundle on cluster using operator sdk.
	oc create ns openshift-lifecycle-agent
	$(OPERATOR_SDK) --security-context-config restricted -n openshift-lifecycle-agent run bundle $(BUNDLE_IMG)

.PHONY: bundle-upgrade
bundle-upgrade: # Upgrade bundle on cluster using operator sdk.
	$(OPERATOR_SDK) run bundle-upgrade $(BUNDLE_IMG)

.PHONY: bundle-clean
bundle-clean: # Uninstall bundle on cluster using operator sdk.
	$(OPERATOR_SDK) cleanup lifecycle-agent -n openshift-lifecycle-agent
	oc delete ns openshift-lifecycle-agent

# A comma-separated list of bundle images (e.g. make catalog-build BUNDLE_IMGS=example.com/operator-bundle:v0.1.0,example.com/operator-bundle:v0.2.0).
# These images MUST exist in a registry and be pull-able.
BUNDLE_IMGS ?= $(BUNDLE_IMG)

# The image tag given to the resulting catalog image (e.g. make catalog-build CATALOG_IMG=example.com/operator-catalog:v0.2.0).
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)

# Set CATALOG_BASE_IMG to an existing catalog image tag to add $BUNDLE_IMGS to that image.
ifneq ($(origin CATALOG_BASE_IMG), undefined)
FROM_INDEX_OPT := --from-index $(CATALOG_BASE_IMG)
endif

# Build a catalog image by adding bundle images to an empty catalog using the operator package manager tool, 'opm'.
# This recipe invokes 'opm' in 'semver' bundle add mode. For more information on add modes, see:
# https://github.com/operator-framework/community-operators/blob/7f1438c/docs/packaging-operator.md#updating-your-existing-operator
.PHONY: catalog-build
catalog-build: opm ## Build a catalog image.
	$(OPM) index add --container-tool $(ENGINE) --mode semver --tag $(CATALOG_IMG) --bundles $(BUNDLE_IMGS) $(FROM_INDEX_OPT)

# Push the catalog image.
.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	${ENGINE} push ${CATALOG_IMG}

##@ lca-cli

cli-run: common-deps-update fmt vet ## Run the lca-cli tool from your host.
	go run main/lca-cli/main.go

cli-build: common-deps-update fmt vet ## Build the lca-cli and ib-cli tools from your host.
	go build -o bin/lca-cli main/lca-cli/main.go
	go build -o bin/ib-cli main/ib-cli/main.go

# Unittests variables
TEST_FORMAT ?= standard-verbose
GOTEST_FLAGS = --format=$(TEST_FORMAT)
GINKGO_FLAGS = -ginkgo.focus="$(FOCUS)" -ginkgo.v -ginkgo.skip="$(SKIP)"

##@ Tools and Linting

.PHONY: lint
lint: bashate golangci-lint shellcheck yamllint markdownlint yq-sort-and-format

.PHONY: tools
tools: opm operator-sdk yq

.PHONY: bashate
bashate: ## Download bashate and lint bash files in the repository
	@echo "Downloading bashate..."
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/download download-bashate DOWNLOAD_INSTALL_DIR=$(PROJECT_DIR)/bin
	@echo "Bashate downloaded successfully."
	@echo "Running bashate on repository bash files..."
	find . -name '*.sh' \
		-not -path './vendor/*' \
		-not -path './*/vendor/*' \
		-not -path './git/*' \
		-not -path './bin/*' \
		-not -path './testbin/*' \
		-not -path './telco5g-konflux/*' \
		-print0 \
		| xargs -0 --no-run-if-empty bashate -v -e 'E*' -i E006
	@echo "Bashate linting completed successfully."

.PHONY: golangci-lint
golangci-lint: ## Run golangci-lint against code.
	@echo "Downloading golangci-lint..."
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/download download-golangci-lint DOWNLOAD_INSTALL_DIR=$(PROJECT_DIR)/bin
	@echo "Golangci-lint downloaded successfully."
	@echo "Running golangci-lint on repository go files..."
	golangci-lint run -v
	@echo "Golangci-lint linting completed successfully."

# markdownlint rules, following: https://github.com/openshift/enhancements/blob/master/Makefile
.PHONY: markdownlint-image
markdownlint-image:  ## Build local container markdownlint-image
	$(ENGINE) image build -f ./hack/Dockerfile.markdownlint --tag $(IMAGE_NAME)-markdownlint:latest ./hack

.PHONY: markdownlint-image-clean
markdownlint-image-clean:  ## Remove locally cached markdownlint-image
	$(ENGINE) image rm $(IMAGE_NAME)-markdownlint:latest

markdownlint: markdownlint-image  ## run the markdown linter
	$(ENGINE) run \
		--rm=true \
		--env RUN_LOCAL=true \
		--env VALIDATE_MARKDOWN=true \
		--env PULL_BASE_SHA=$(PULL_BASE_SHA) \
		-v $$(pwd):/workdir:Z \
		$(IMAGE_NAME)-markdownlint:latest

operator-sdk: ## Download operator-sdk locally if necessary.
	@$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/download download-operator-sdk \
		DOWNLOAD_INSTALL_DIR=$(PROJECT_DIR)/bin \
		DOWNLOAD_OPERATOR_SDK_VERSION=$(OPERATOR_SDK_VERSION)
	@echo "Operator sdk downloaded successfully."

.PHONY: opm
opm: ## Download opm locally if necessary.
	@$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/download download-opm \
		DOWNLOAD_INSTALL_DIR=$(PROJECT_DIR)/bin \
		DOWNLOAD_OPM_VERSION=$(OPM_VERSION)
	@echo "Opm downloaded successfully."

.PHONY: shellcheck
shellcheck: ## Download shellcheck and lint bash files in the repository
	@echo "Downloading shellcheck..."
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/download download-shellcheck DOWNLOAD_INSTALL_DIR=$(PROJECT_DIR)/bin
	@echo "Shellcheck downloaded successfully."
	@echo "Running shellcheck on repository bash files..."
	find . -name '*.sh' \
		-not -path './vendor/*' \
		-not -path './*/vendor/*' \
		-not -path './git/*' \
		-not -path './bin/*' \
		-not -path './testbin/*' \
		-not -path './telco5g-konflux/*' \
		-print0 \
		| xargs -0 --no-run-if-empty shellcheck -x
	@echo "Shellcheck linting completed successfully."

.PHONY: yamllint
yamllint: ## Download yamllint and lint YAML files in the repository
	@echo "Downloading yamllint..."
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/download download-yamllint DOWNLOAD_INSTALL_DIR=$(PROJECT_DIR)/bin
	@echo "Yamllint downloaded successfully."
	@echo "Running yamllint on repository YAML files..."
	yamllint -c .yamllint.yaml .
	@echo "Yamllint linting completed successfully."

.PHONY: yq
yq: ## Download yq
	@echo "Downloading yq..."
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/download download-yq DOWNLOAD_INSTALL_DIR=$(PROJECT_DIR)/bin
	@echo "Yq downloaded successfully."

.PHONY: yq-sort-and-format
yq-sort-and-format: yq ## Sort keys/reformat all YAML files in the repository
	@echo "Sorting keys and reformatting YAML files..."
	@find . -name "*.yaml" -o -name "*.yml" | grep -v -E "(telco5g-konflux/|target/|vendor/|bin/|\.git/)" | while read file; do \
		echo "Processing $$file..."; \
		yq -i '.. |= sort_keys(.)' "$$file"; \
	done
	@echo "YAML sorting and formatting completed successfully."

##@ Konflux

.PHONY: konflux-validate-catalog-template-bundle ## validate the last bundle entry on the catalog template file
konflux-validate-catalog-template-bundle: yq operator-sdk
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/catalog konflux-validate-catalog-template-bundle \
		CATALOG_TEMPLATE_KONFLUX=$(PROJECT_DIR)/$(CATALOG_TEMPLATE_KONFLUX) \
		YQ=$(YQ) \
		OPERATOR_SDK=$(OPERATOR_SDK) \
		ENGINE=$(ENGINE)

.PHONY: konflux-validate-catalog
konflux-validate-catalog: opm ## validate the current catalog file
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/catalog konflux-validate-catalog \
		CATALOG_KONFLUX=$(PROJECT_DIR)/$(CATALOG_KONFLUX) \
		OPM=$(OPM)

.PHONY: konflux-generate-catalog ## generate a quay.io catalog
konflux-generate-catalog: yq opm
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/catalog konflux-generate-catalog-legacy \
		CATALOG_TEMPLATE_KONFLUX=$(PROJECT_DIR)/$(CATALOG_TEMPLATE_KONFLUX) \
		CATALOG_KONFLUX=$(PROJECT_DIR)/$(CATALOG_KONFLUX) \
		PACKAGE_NAME_KONFLUX=$(PACKAGE_NAME_KONFLUX) \
		BUNDLE_BUILDS_FILE=$(PROJECT_DIR)/.konflux/catalog/bundle.builds.in.yaml \
		OPM=$(OPM) \
		YQ=$(YQ)
	$(MAKE) konflux-validate-catalog

.PHONY: konflux-generate-catalog-production ## generate a registry.redhat.io catalog
konflux-generate-catalog-production: yq opm
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/catalog konflux-generate-catalog-production-legacy \
		CATALOG_TEMPLATE_KONFLUX=$(PROJECT_DIR)/$(CATALOG_TEMPLATE_KONFLUX) \
		CATALOG_KONFLUX=$(PROJECT_DIR)/$(CATALOG_KONFLUX) \
		PACKAGE_NAME_KONFLUX=$(PACKAGE_NAME_KONFLUX) \
		BUNDLE_NAME_SUFFIX=$(BUNDLE_NAME_SUFFIX) \
		PRODUCTION_BUNDLE_NAME=$(PRODUCTION_BUNDLE_NAME) \
		BUNDLE_BUILDS_FILE=$(PROJECT_DIR)/.konflux/catalog/bundle.builds.in.yaml \
		OPM=$(OPM) \
		YQ=$(YQ)
	$(MAKE) konflux-validate-catalog

.PHONY: konflux-filter-unused-redhat-repos
konflux-filter-unused-redhat-repos: ## Filter unused repositories from redhat.repo files in runtime lock folder
	@echo "Filtering unused repositories from runtime lock folder..."
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/rpm-lock filter-unused-repos REPO_FILE=$(PROJECT_DIR)/.konflux/lock-runtime/redhat.repo
	@echo "Filtering completed for runtime lock folder."

.PHONY: konflux-update-tekton-task-refs
konflux-update-tekton-task-refs: ## Update task references in Tekton pipeline files
	@echo "Updating task references in Tekton pipeline files..."
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/tekton update-task-refs PIPELINE_FILES="$(shell find $(PROJECT_DIR)/.tekton -name '*.yaml' -not -name 'OWNERS' | tr '\n' ' ')"
	@echo "Task references updated successfully."

.PHONY: konflux-compare-catalog
konflux-compare-catalog: ## Compare generated catalog with upstream FBC image
	@echo "Comparing generated catalog with upstream FBC image..."
	$(MAKE) -C $(PROJECT_DIR)/telco5g-konflux/scripts/catalog konflux-compare-catalog \
		CATALOG_KONFLUX=$(PROJECT_DIR)/$(CATALOG_KONFLUX) \
		PACKAGE_NAME_KONFLUX=$(PACKAGE_NAME_KONFLUX) \
		UPSTREAM_FBC_IMAGE=quay.io/redhat-user-workloads/telco-5g-tenant/$(PACKAGE_NAME_KONFLUX)-fbc-4-20:latest

.PHONY: konflux-all
konflux-all: konflux-filter-unused-redhat-repos konflux-update-tekton-task-refs konflux-generate-catalog-production konflux-validate-catalog ## Run all Konflux-related targets
	@echo "All Konflux targets completed successfully."

help:   ## Shows this message.
	@echo "Available targets:"
	@awk 'BEGIN {FS = ":.*?## "}; /^[a-zA-Z0-9_-]+:.*?## / {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

clean:
	rm -rf $(PROJECT_DIR)/bin/
