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
COPY ibu-imager ibu-imager
COPY internal internal
COPY main main
COPY utils utils

# Build the binaries
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/manager main/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/ibu-imager main/ibu-imager/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/precache main/precache-workload/main.go

#####################################################################################################
# Build the operator image
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

RUN if [[ ! -f /bin/nsenter ]]; then \
        microdnf -y install util-linux-core && \
        microdnf clean all && \
        rm -rf /var/cache/yum ; \
    fi

COPY --from=builder \
    /opt/app-root/src/build/manager \
    /opt/app-root/src/build/ibu-imager \
    /opt/app-root/src/build/precache \
    /usr/local/bin/

COPY ibu-imager/installation_configuration_files/ /usr/local/installation_configuration_files/

ENTRYPOINT ["/usr/local/bin/manager"]
