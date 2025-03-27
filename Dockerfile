#####################################################################################################
# Build arguments
ARG BUILDER_IMAGE=quay.io/projectquay/golang:1.23
ARG RUNTIME_IMAGE=registry.access.redhat.com/ubi9-minimal:9.4
ARG ORIGIN_CLI_IMAGE=quay.io/openshift/origin-cli-artifacts:latest

# Assume x86 unless otherwise specified
ARG GOARCH="amd64"

# Build the binaries
FROM ${BUILDER_IMAGE} as builder

# Pass GOARCH into builder
ARG GOARCH

# Explicitly set the working directory
WORKDIR /opt/app-root

# Bring in the go dependencies before anything else so we can take
# advantage of caching these layers in future builds.
COPY vendor/ vendor/

# Copy the go modules manifests
COPY go.* .

# Copy the required source directories
COPY api api
COPY controllers controllers
COPY lca-cli lca-cli
COPY internal internal
COPY main main
COPY utils utils

# Build the binaries
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${GOARCH} GO111MODULE=on go build -mod=vendor -a -o build/manager main/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${GOARCH} GO111MODULE=on go build -mod=vendor -a -o build/lca-cli main/lca-cli/main.go

#####################################################################################################
# Build the operator image
# note: update origin-cli-artifacts from `latest` to an appropriate OCP verison during release e.g `4.18`
FROM ${ORIGIN_CLI_IMAGE} AS origincli
FROM ${RUNTIME_IMAGE}

# Pass GOARCH into runtime
ARG GOARCH

# Explicitly set the working directory
WORKDIR /

RUN if [[ ! -f /bin/nsenter ]]; then \
        microdnf -y install util-linux-core && \
        microdnf -y install rsync && \
        microdnf -y install tar && \
        microdnf clean all && \
        rm -rf /var/cache/yum ; \
    fi


COPY --from=builder \
    /opt/app-root/build/manager \
    /opt/app-root/build/lca-cli \
    /usr/local/bin/

COPY lca-cli/installation_configuration_files/ /usr/local/installation_configuration_files/

COPY --from=origincli /usr/share/openshift/linux_${GOARCH}/oc.rhel9 /usr/bin/oc

COPY must-gather/collection-scripts/ /usr/bin/

ENTRYPOINT ["/usr/local/bin/manager"]
