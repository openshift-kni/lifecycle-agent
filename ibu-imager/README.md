# IBU Imager

[![CI](https://github.com/leo8a/ibu-imager/actions/workflows/pull_request_workflow.yml/badge.svg)](https://github.com/leo8a/ibu-imager/actions/workflows/pull_request_workflow.yml)

This application will assist users to easily create an OCI seed image for the Image-Based Upgrade (IBU) workflow, using 
a simple CLI.

## Motivation

One of the most important / critical day2 operations for Telecommunications cloud-native deployments is upgrading their
Container-as-a-Service (CaaS) platforms as quick (and secure!) as possible while minimizing the disruption time of 
active workloads.

A novel method to approach this problem can be derived based on the
[CoreOS Layering](https://github.com/coreos/enhancements/blob/main/os/coreos-layering.md) concepts, which proposes a 
new way of updating the underlying Operating System (OS) using OCI-compliant container images.

This tool aims at creating such OCI images plus bundling the main cluster artifacts and configurations in order to 
provide seed images that can be used during an image-based upgrade procedure that would drastically reduce the 
upgrading and reconfiguration times.  

### What does this tool do?

The purpose of the `ibu-imager` tool is to assist in the creation of IBU seed images, which are used later on by 
other components (e.g., [lifecycle-agent](https://github.com/openshift-kni/lifecycle-agent)) during an image-based 
upgrade procedure.

In that direction, the tool does the following: 

- Saves a list of container images used by `crio` (needed for pre-caching operations afterward)
- Creates a backup of the main platform configurations (e.g., `/var` and `/etc` directories, ostree artifacts, etc.)
- Encapsulates OCI images (as of now three: `backup`, `base`, and `parent`) and push them to a local registry (used 
during the image-based upgrade workflow afterward)

### Building

Building the binary locally.

```shell
-> make build 
go mod tidy && go mod vendor
Running go fmt
go fmt ./...
Running go vet
go vet ./...
go build -o bin/ibu-imager main.go
```

> **Note:** The binary can be found in `./bin/ibu-imager`.

Building and pushing the tool as container image.

```shell
-> make docker-build docker-push
podman build -t quay.io/lochoa/ibu-imager:4.14.0 -f Dockerfile .
[1/2] STEP 1/9: FROM registry.hub.docker.com/library/golang:1.19 AS builder
Trying to pull registry.hub.docker.com/library/golang:1.19...
Getting image source signatures
Copying blob 5ec11cb68eac done   | 
Copying blob 012c0b3e998c done   | 
Copying blob 9f13f5a53d11 done   | 
Copying blob 00046d1e755e done   | 
Copying blob 190fa1651026 done   | 
Copying blob 0808c6468790 done   | 
Copying config 80b76a6c91 done   | 
Writing manifest to image destination
[1/2] STEP 2/9: ENV CRIO_VERSION="v1.28.0"
--> f33ad7e8ba50
[1/2] STEP 3/9: WORKDIR /workspace
--> ea41fd2db1a9
[1/2] STEP 4/9: COPY go.mod go.sum ./
--> 92a2056f63b9
[1/2] STEP 5/9: COPY vendor/ vendor/
--> 10aaa8c0e678
[1/2] STEP 6/9: COPY main.go main.go
--> bef5739828bf
[1/2] STEP 7/9: COPY cmd/ cmd/
--> 6344ed52a7e9
[1/2] STEP 8/9: RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -mod=vendor -a -o ibu-imager main.go
--> dbe5da0d5b09
[1/2] STEP 9/9: RUN curl -sL https://github.com/kubernetes-sigs/cri-tools/releases/download/$CRIO_VERSION/crictl-$CRIO_VERSION-linux-amd64.tar.gz         | tar xvzf - -C . && chmod +x ./crictl
crictl
--> aece8a3afed3
[2/2] STEP 1/6: FROM registry.access.redhat.com/ubi9/ubi:latest
Trying to pull registry.access.redhat.com/ubi9/ubi:latest...
Getting image source signatures
Checking if image destination supports signatures
Copying blob cc7c08d56aad done   | 
Copying config 20cef05760 done   | 
Writing manifest to image destination
Storing signatures
[2/2] STEP 2/6: WORKDIR /
--> dd42896a7332
[2/2] STEP 3/6: COPY --from=builder /workspace/ibu-imager .
--> 2d4c7b6153c1
[2/2] STEP 4/6: COPY --from=builder /workspace/crictl /usr/bin/
--> 7300e8f6820b
[2/2] STEP 5/6: RUN yum -y install jq &&     yum clean all &&     rm -rf /var/cache/yum
[2/2] STEP 6/6: ENTRYPOINT ["./ibu-imager"]
[2/2] COMMIT quay.io/lochoa/ibu-imager:4.14.0
--> 4f070f5dc851
Successfully tagged quay.io/lochoa/ibu-imager:4.14.0
4f070f5dc851ef5476287fe4a8192f0d6969fc6471cb0e753d5cdd13b6a9c3b6
podman push quay.io/lochoa/ibu-imager:4.14.0
Getting image source signatures
Copying blob 2b35b9c14c2a done   | 
Copying blob 6f740e943089 done   | 
Copying blob bcfbd6cf92f8 done   | 
Copying blob 13843eae2086 done   | 
Copying config 4f070f5dc8 done   | 
Writing manifest to image destination
```

### Running the tool's help

To see the tool's help on your local host, run the following command:

```shell
-> ./bin/ibu-imager -h

 ___ ____  _   _            ___                                 
|_ _| __ )| | | |          |_ _|_ __ ___   __ _  __ _  ___ _ __ 
 | ||  _ \| | | |   _____   | ||  _   _ \ / _  |/ _  |/ _ \ '__|
 | || |_) | |_| |  |_____|  | || | | | | | (_| | (_| |  __/ |
|___|____/ \___/           |___|_| |_| |_|\__,_|\__, |\___|_|
                                                |___/

 A tool to assist building OCI seed images for Image Based Upgrades (IBU)

Usage:
  ibu-imager [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  create      Create OCI image and push it to a container registry.
  help        Help about any command

Flags:
  -h, --help       help for ibu-imager
  -c, --no-color   Control colored output
  -v, --verbose    Display verbose logs

Use "ibu-imager [command] --help" for more information about a command.
```

### Running as a container

To create an IBU seed image out of your Single Node OpenShift (SNO), run the following command directly on the node:

```shell
-> podman run --privileged --pid=host --rm --net=host \
				-v /etc:/etc \
 				-v /var:/var \
 				-v /var/run:/var/run \
 				-v /run/systemd/journal/socket:/run/systemd/journal/socket \
 				quay.io/lochoa/ibu-imager:4.14.0 create --authfile /var/lib/kubelet/config.json --registry ${LOCAL_USER_REGISTRY}

 ___ ____  _   _            ___                                 
|_ _| __ )| | | |          |_ _|_ __ ___   __ _  __ _  ___ _ __ 
 | ||  _ \| | | |   _____   | ||  _   _ \ / _  |/ _  |/ _ \ '__|
 | || |_) | |_| |  |_____|  | || | | | | | (_| | (_| |  __/ |
|___|____/ \___/           |___|_| |_| |_|\__,_|\__, |\___|_|
                                                |___/

 A tool to assist building OCI seed images for Image Based Upgrades (IBU)
	
time="2023-09-22 10:18:58" level=info msg="OCI image creation has started"
time="2023-09-22 10:18:58" level=info msg="Saving list of running containers and clusterversion."
time="2023-09-22 10:18:58" level=info msg="List of containers, catalogsources, and clusterversion saved successfully."
time="2023-09-22 10:18:58" level=info msg="Stop kubelet service"
time="2023-09-22 10:18:58" level=info msg="Stopping containers and CRI-O runtime."
time="2023-09-22 10:18:58" level=info msg="Running containers and CRI-O engine stopped successfully."
time="2023-09-22 10:18:58" level=info msg="Create backup datadir"
time="2023-09-22 10:18:58" level=info msg="Backup of /var created successfully."
time="2023-09-22 10:18:58" level=info msg="Backup of /etc created successfully."
time="2023-09-22 10:18:58" level=info msg="Backup of ostree created successfully."
time="2023-09-22 10:18:58" level=info msg="Backup of rpm-ostree.json created successfully."
time="2023-09-22 10:18:58" level=info msg="Backup of mco-currentconfig created successfully."
time="2023-09-22 10:19:05" level=info msg="Backup of .origin created successfully."
time="2023-09-22 10:21:45" level=info msg="Encapsulate and push parent OCI image."
time="2023-09-22 10:24:21" level=info msg="OCI image created successfully!"
```

> **Note:** For a disconnected environment, first mirror the `ibu-imager` container image to your local registry using 
> [skopeo](https://github.com/containers/skopeo) or a similar tool.

## TODO

<details>
  <summary>TODO List</summary>

- [ ] Refactor wrapped bash commands (e.g., rpm-ostree commands) with stable go-bindings and/or libraries
- [ ] Fix all code TODO comments

</details>

## Contributors

IBU Imager is built and maintained by our growing community of contributors 🏆!

<a href="https://github.com/leo8a/ibu-imager/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=leo8a/ibu-imager" />
</a>

Made with [contributors-img](https://contrib.rocks).
