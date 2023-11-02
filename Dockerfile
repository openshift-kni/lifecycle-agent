# Build the manager binary
FROM registry.hub.docker.com/library/golang:1.20 as builder

WORKDIR /workspace/

# Bring in the go dependencies before anything else so we can take
# advantage of caching these layers in future builds.
COPY vendor/ vendor/

# Copy the go source
COPY . /workspace

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/manager main/main.go

#####################################################################################################
# Build the imager binary
FROM registry.hub.docker.com/library/golang:1.20 as imager

WORKDIR /workspace

# Bring in the go dependencies before anything else so we can take
# advantage of caching these layers in future builds.
COPY vendor/ vendor/

# Copy the Go Modules manifests
COPY . /workspace

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o build/ibu-imager main/ibu-imager/main.go


#####################################################################################################
# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
#FROM gcr.io/distroless/static:nonroot
FROM registry.access.redhat.com/ubi9/ubi:latest

RUN yum -y install jq && \
    yum clean all && \
    rm -rf /var/cache/yum

WORKDIR /

COPY --from=builder /workspace/build/manager .

COPY --from=imager /workspace/build/ibu-imager .
COPY ibu-imager/installation_configuration_files/ installation_configuration_files/

ENTRYPOINT ["/manager"]
