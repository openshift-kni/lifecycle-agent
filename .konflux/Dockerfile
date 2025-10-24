# Build the binaries
FROM brew.registry.redhat.io/rh-osbs/openshift-golang-builder:rhel_9_golang_1.24@sha256:8412323224878c4e27cd6f7dc6ce99c50f878f37df323285bff29a53f8ec37cd as builder

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

# Compile binaries with FIPS enabled
RUN GOEXPERIMENT=strictfipsruntime CGO_ENABLED=1 GOOS=linux GO111MODULE=on go build -mod=vendor -tags strictfipsruntime -o build/manager main/main.go && \
    GOEXPERIMENT=strictfipsruntime CGO_ENABLED=1 GOOS=linux GO111MODULE=on go build -mod=vendor -tags strictfipsruntime -a -o build/lca-cli main/lca-cli/main.go;

#####################################################################################################
# Build the operator image
# When 4.21 is released, we need to update the openshift-cli image to v4.21
FROM registry.redhat.io/openshift4/ose-cli-rhel9:v4.20@sha256:c69995f11a43e6173a2d05d5912f391488655cce7f0507cde51325fcd1e36911 AS openshift-cli
#

# For Konflux we will use RHEL-els as the base image
FROM registry.redhat.io/rhel9-6-els/rhel-minimal:9.6@sha256:2bdf19e0877fdbb0696330159ed72a12deef8a60eed819c4e2fc0064147b39df as runtime-image
#

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

ENTRYPOINT ["/usr/local/bin/manager"]
