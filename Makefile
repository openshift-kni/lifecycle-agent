# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= 4.14.0

# You can use podman or docker as a container engine. Notice that there are some options that might be only valid for one of them.
ENGINE ?= docker

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
OPERATOR_SDK = $(shell pwd)/bin/x86_64/operator-sdk
KUSTOMIZE = $(shell pwd)/bin/kustomize
GOTESTSUM = $(shell pwd)/bin/gotestsum
MOCK_GEN = $(shell pwd)/bin/mockgen
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

.PHONY: golangci-lint
golangci-lint: ## Run golangci-lint against code.
	@echo "Running golangci-lint"
	hack/golangci-lint.sh

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

.PHONY: shellcheck
shellcheck: ## Run shellcheck.
	@echo "Running shellcheck"
	hack/shellcheck.sh

.PHONY: bashate
bashate: ## Run bashate.
	@echo "Running bashate"
	hack/bashate.sh

.PHONY: ci-job
ci-job: common-deps-update generate fmt vet golangci-lint unittest shellcheck bashate bundle-check

kustomize: ## Download kustomize locally if necessary.
	$(call go-get-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5@v5.1.1)

OPERATOR_SDK_VERSION = $(shell $(OPERATOR_SDK) version 2>/dev/null | sed 's/^operator-sdk version: "\([^"]*\).*/\1/')
OPERATOR_SDK_VERSION_REQ = v1.28.0-ocp
operator-sdk: ## Download operator-sdk locally if necessary.
ifneq ($(OPERATOR_SDK_VERSION_REQ),$(OPERATOR_SDK_VERSION))
	curl -L https://mirror.openshift.com/pub/openshift-v4/x86_64/clients/operator-sdk/4.13.11/operator-sdk-v1.28.0-ocp-linux-x86_64.tar.gz | tar -xz -C bin/
endif

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
	${ENGINE} build -t ${IMG} -f Dockerfile .

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
	sed -i '/^[[:space:]]*createdAt:/d' bundle/manifests/lifecycle-agent.clusterserviceversion.yaml

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
bundle-run: # Install bundle on cluster using operator sdk. Index image is require due to upstream issue: https://github.com/operator-framework/operator-registry/issues/984
	oc create ns openshift-lifecycle-agent
	$(OPERATOR_SDK) --index-image=quay.io/operator-framework/opm:v1.28.0 --security-context-config restricted -n openshift-lifecycle-agent run bundle $(BUNDLE_IMG)

.PHONY: bundle-upgrade
bundle-upgrade: # Upgrade bundle on cluster using operator sdk.
	$(OPERATOR_SDK) run bundle-upgrade $(BUNDLE_IMG)

.PHONY: bundle-clean
bundle-clean: # Uninstall bundle on cluster using operator sdk.
	$(OPERATOR_SDK) cleanup lifecycle-agent -n openshift-lifecycle-agent
	oc delete ns openshift-lifecycle-agent

.PHONY: opm
OPM = ./bin/opm
opm: ## Download opm locally if necessary.
ifeq (,$(wildcard $(OPM)))
ifeq (,$(shell which opm 2>/dev/null))
	@{ \
	set -e ;\
	mkdir -p $(dir $(OPM)) ;\
	OS=$(shell go env GOOS) && ARCH=$(shell go env GOARCH) && \
	curl -sSLo $(OPM) https://github.com/operator-framework/operator-registry/releases/download/v1.28.0/$${OS}-$${ARCH}-opm ;\
	chmod +x $(OPM) ;\
	}
else
OPM = $(shell which opm)
endif
endif

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
	$(MAKE) docker-push IMG=$(CATALOG_IMG)

##@ lca-cli

cli-run: common-deps-update fmt vet ## Run the lca-cli tool from your host.
	go run main/lca-cli/main.go

cli-build: common-deps-update fmt vet ## Build the lca-cli tool from your host.
	go build -o bin/lca-cli main/lca-cli/main.go

# Unittests variables
TEST_FORMAT ?= standard-verbose
GOTEST_FLAGS = --format=$(TEST_FORMAT)
GINKGO_FLAGS = -ginkgo.focus="$(FOCUS)" -ginkgo.v -ginkgo.skip="$(SKIP)"

# markdownlint rules, following: https://github.com/openshift/enhancements/blob/master/Makefile
.PHONY: markdownlint-image
markdownlint-image:  ## Build local container markdownlint-image
	$(ENGINE) image build -f ./hack/Dockerfile.markdownlint --tag $(IMAGE_NAME)-markdownlint:latest

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

help:   ## Shows this message.
	@echo "Available targets:"
	@awk 'BEGIN {FS = ":.*?## "}; /^[a-zA-Z0-9_-]+:.*?## / {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
