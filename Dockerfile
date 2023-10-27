# Build the manager binary
FROM registry.hub.docker.com/library/golang:1.19 as builder

WORKDIR /workspace

# Copy the Go Modules manifests
COPY go.mod go.sum ./
COPY vendor/ vendor/

# Copy the go source
COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/
COPY internal/ internal/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o manager main.go

#####################################################################################################
# Build the imager binary
FROM registry.hub.docker.com/library/golang:1.19 as imager

ENV CRIO_VERSION="v1.28.0"

WORKDIR /workspace
# Copy the Go Modules manifests
COPY ibu-imager/go.mod ibu-imager/go.sum ./
COPY ibu-imager/vendor/ vendor/

# Copy the Go source installation_configuration_files
COPY ibu-imager/main.go main.go
COPY ibu-imager/cmd/ cmd/
COPY ibu-imager/internal/ internal/

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o ibu-imager main.go

# Download crio CLI
RUN curl -sL https://github.com/kubernetes-sigs/cri-tools/releases/download/$CRIO_VERSION/crictl-$CRIO_VERSION-linux-amd64.tar.gz \
        | tar xvzf - -C . && chmod +x ./crictl

#####################################################################################################
# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
#FROM gcr.io/distroless/static:nonroot
FROM registry.access.redhat.com/ubi9/ubi:latest

RUN yum -y install jq && \
    yum clean all && \
    rm -rf /var/cache/yum

WORKDIR /

COPY --from=builder /workspace/manager .

COPY --from=imager /workspace/ibu-imager .
COPY --from=imager /workspace/crictl /usr/bin/
COPY ibu-imager/installation_configuration_files/ installation_configuration_files/

ENTRYPOINT ["/manager"]
