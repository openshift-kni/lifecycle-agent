#####################################################################################################
# Build the manager binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.20 as builder

# Bring in the go dependencies before anything else so we can take
# advantage of caching these layers in future builds.
COPY vendor/ vendor/

# Copy the go modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# Copy the go source
COPY api api
COPY controllers controllers
COPY internal internal
COPY ibu-imager ibu-imager
COPY utils utils
COPY main main

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/manager main/main.go


#####################################################################################################
# Build the imager binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.20 as imager

# Bring in the go dependencies before anything else so we can take
# advantage of caching these layers in future builds.
COPY vendor/ vendor/

# Copy the go modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# Copy the go source
COPY main main
COPY utils utils
COPY controllers/utils controllers/utils
COPY api/v1alpha1 api/v1alpha1
COPY internal internal
COPY ibu-imager ibu-imager

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/ibu-imager main/ibu-imager/main.go

#####################################################################################################
# Build the workload binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.20 as precache-workload

# Bring in the go dependencies before anything else so we can take
# advantage of caching these layers in future builds.
COPY vendor/ vendor/

# Copy the go modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# Copy the go source
COPY api/v1alpha1 api/v1alpha1
COPY controllers/utils controllers/utils
COPY internal internal
COPY ibu-imager ibu-imager
COPY utils utils
COPY main/precache-workload main/precache-workload

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/precache main/precache-workload/main.go

#####################################################################################################
# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
#FROM gcr.io/distroless/static:nonroot
FROM registry.access.redhat.com/ubi9/ubi:latest

RUN dnf -y install jq && \
    dnf clean all && \
    rm -rf /var/cache/dnf

COPY --from=builder /opt/app-root/src/build/manager /usr/local/bin/manager

COPY --from=imager /opt/app-root/src/build/ibu-imager /usr/local/bin/ibu-imager
COPY ibu-imager/installation_configuration_files/ /usr/local/installation_configuration_files/

COPY --from=precache-workload /opt/app-root/src/build/precache /usr/local/bin/precache

ENTRYPOINT ["/usr/local/bin/manager"]
