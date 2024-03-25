#####################################################################################################
# Build the binaries
FROM registry.access.redhat.com/ubi9/go-toolset:1.20 as builder

# Bring in the go dependencies before anything else so we can take
# advantage of caching these layers in future builds.
COPY vendor/ vendor/

# Copy the go modules manifests
COPY go.* .

# Copy the required source directories
COPY api api
COPY controllers controllers
COPY lca-cli lca-cli
COPY ib-cli ib-cli
COPY internal internal
COPY main main
COPY utils utils

# Build the binaries
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/manager main/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/lca-cli main/lca-cli/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/ib-cli main/ib-cli/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/precache main/precache-workload/main.go

#####################################################################################################
# Build the operator image
# note: update origin-cli-artifacts from `latest` to an appropriate OCP verison during release e.g `4.16`
FROM quay.io/openshift/origin-cli-artifacts:latest AS origincli
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

RUN if [[ ! -f /bin/nsenter ]]; then \
        microdnf -y install util-linux-core && \
        microdnf -y install rsync && \
        microdnf -y install tar && \
        microdnf clean all && \
        rm -rf /var/cache/yum ; \
    fi

COPY --from=builder \
    /opt/app-root/src/build/manager \
    /opt/app-root/src/build/lca-cli \
    /opt/app-root/src/build/ib-cli \
    /opt/app-root/src/build/precache \
    /usr/local/bin/

COPY lca-cli/installation_configuration_files/ /usr/local/installation_configuration_files/

COPY --from=origincli /usr/share/openshift/linux_amd64/oc.rhel9 /usr/bin/oc

COPY must-gather/collection-scripts/ /usr/bin/

ENTRYPOINT ["/usr/local/bin/manager"]
