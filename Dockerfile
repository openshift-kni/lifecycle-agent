#####################################################################################################
# Build arguments
ARG BUILDER_IMAGE=quay.io/projectquay/golang:1.23
ARG OPENSHIFT_CLI_IMAGE=registry.redhat.io/openshift4/ose-cli-rhel9:latest
ARG RUNTIME_IMAGE=registry.access.redhat.com/ubi9-minimal:9.4
ARG YQ_IMAGE=quay.io/konflux-ci/yq:latest

# Assume x86 unless otherwise specified
ARG GOARCH="amd64"

# Default Konflux to false
ARG KONFLUX="false"

#####################################################################################################
# Build the binaries
FROM --platform=linux/${GOARCH} ${BUILDER_IMAGE} as builder

# Pass arguments into layer
ARG GOARCH
ARG KONFLUX

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

# For Konflux, compile with FIPS enabled
# Otherwise compile normally
RUN if [[ "${KONFLUX}" == "true" ]]; then \
        echo "Compiling with fips" && \
        GOEXPERIMENT=strictfipsruntime CGO_ENABLED=1 GOOS=linux GOARCH=${GOARCH} GO111MODULE=on go build -mod=vendor -tags strictfipsruntime -o build/manager main/main.go && \
        GOEXPERIMENT=strictfipsruntime CGO_ENABLED=1 GOOS=linux GOARCH=${GOARCH} GO111MODULE=on go build -mod=vendor -tags strictfipsruntime -a -o build/lca-cli main/lca-cli/main.go; \
    else \
        echo "Compiling without fips" && \
        CGO_ENABLED=0 GOOS=linux GOARCH=${GOARCH} GO111MODULE=on go build -mod=vendor -a -o build/manager main/main.go && \
        CGO_ENABLED=0 GOOS=linux GOARCH=${GOARCH} GO111MODULE=on go build -mod=vendor -a -o build/lca-cli main/lca-cli/main.go; \
    fi

#####################################################################################################
# We have to scrape the recert image out of the CSV file
FROM ${YQ_IMAGE} AS overlay

# Pass argument into layer
ARG KONFLUX

# Set work dir
WORKDIR /tmp

# Copy the Konflux overlay pinning file since it contains both the default image and the Konflux image
COPY .konflux/overlay/pin_images.in.yaml .

# Prepare the configuration file
RUN echo -n "RELATED_IMAGE_RECERT_IMAGE=" > /tmp/recert_image.sh

# Extract the final recert key
RUN if [[ "${KONFLUX}" == "true" ]]; then \
        yq '.[] | select(.key == "recert") | .target' /tmp/pin_images.in.yaml >> /tmp/recert_image.sh; \
    else \
        yq '.[] | select(.key == "recert") | .source' /tmp/pin_images.in.yaml >> /tmp/recert_image.sh; \
    fi

# Export the variable and make sure the script is executable for later
RUN echo "export RELATED_IMAGE_RECERT_IMAGE" >> /tmp/recert_image.sh && \
    chmod +x /tmp/recert_image.sh

#####################################################################################################
# Build the operator image
FROM --platform=linux/${GOARCH} ${OPENSHIFT_CLI_IMAGE} AS openshift-cli
FROM --platform=linux/${GOARCH} ${RUNTIME_IMAGE} as runtime-image

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

COPY --from=openshift-cli /usr/bin/oc /usr/bin/oc

COPY must-gather/collection-scripts/ /usr/bin/

# The entrypoint script will set the environment variable for `RELATED_IMAGE_RECERT_IMAGE` and then start the binary
COPY --from=overlay /tmp/recert_image.sh /recert_image.sh
COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
