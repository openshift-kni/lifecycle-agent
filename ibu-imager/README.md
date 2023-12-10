# IBU Imager

This application will assist users to easily create an OCI seed image for the Image-Based Upgrade (IBU) workflow, using
a simple CLI.

## Motivation

One of the most important / critical day2 operations for Telecommunications cloud-native deployments is upgrading their
Container-as-a-Service (CaaS) platforms as quick (and secure!) as possible while minimizing the disruption time of
active workloads.

A novel method to approach this problem can be derived based on the
[CoreOS Layering](https://github.com/coreos/enhancements/blob/main/os/coreos-layering.md) concepts, which proposes a
new way of updating the underlying Operating System (OS) using OCI-compliant container images.

In this context, this tool aims at creating such OCI images, and bundling the main cluster artifacts / configurations
in order to
provide seed images that can be used during an image-based upgrade procedure that would drastically reduce the
upgrading and reconfiguration times.

### What does this tool do?

The purpose of the `ibu-imager` tool is to assist in the creation of IBU seed images, which are used later on by
other components (e.g., [lifecycle-agent](https://github.com/openshift-kni/lifecycle-agent)) during an image-based
upgrade procedure.

In that direction, the tool does the following at a high level:

- Saves a list of container images used by `crio` (needed for pre-caching operations afterward)
- Creates a backup of the main platform configurations (e.g., `/var` and `/etc` directories, ostree artifacts, etc.)
- Creates a backup of the ostree repository
- Creates a seed container image (OCI) with all the generated content and pushes it to a remote container registry
  (used during the image-based upgrade workflow afterward)

### Building

Building the binary locally.

```shell
-> make imager-build 
go mod tidy
Running go fmt
go fmt ./...
Running go vet
go vet ./...
go build -o bin/ibu-imager main/ibu-imager/main.go
```

> **Note:** The binary can be found in `./bin/ibu-imager`.

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

 A tool to assist in building OCI seed images for Image Based Upgrades (IBU)

Usage:
  ibu-imager [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  create      Create OCI image and push it to a container registry.
  help        Help about any command
  post-pivot  post pivot configuration
  restore     Restore seed cluster configurations

Flags:
  -h, --help       help for ibu-imager
  -c, --no-color   Control colored output
  -v, --verbose    Display verbose logs

Use "ibu-imager [command] --help" for more information about a command.
```

### Running as a container

To create an IBU seed image out of your Single Node OpenShift (SNO), run the following command directly on the node:

```shell
-> export LCA_IMAGE=$(oc get deployment -n openshift-lifecycle-agent lifecycle-agent-controller-manager -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')

-> export AUTHFILE=/path/to/pull-secret.json
-> export SEED_IMG_REFSPEC=quay.io/${MY_REPO_ID}/${MY_REPO}:${MY_TAG}
-> export IMG_RECERT_TOOL=quay.io/edge-infrastructure/recert:latest

-> podman run --privileged --pid=host --rm --net=host \
    -v /etc:/etc \
    -v /var:/var \
    -v /var/run:/var/run \
    -v /run/systemd/journal/socket:/run/systemd/journal/socket \
    -v ${AUTHFILE}:${AUTHFILE} \
    --entrypoint ibu-imager ${LCA_IMAGE} create --authfile ${AUTHFILE} \
                                                 --image ${SEED_IMG_REFSPEC} \
                                                 --recert-image ${IMG_RECERT_TOOL}

 ___ ____  _   _            ___
|_ _| __ )| | | |          |_ _|_ __ ___   __ _  __ _  ___ _ __
 | ||  _ \| | | |   _____   | ||  _   _ \ / _  |/ _  |/ _ \ '__|
 | || |_) | |_| |  |_____|  | || | | | | | (_| | (_| |  __/ |
|___|____/ \___/           |___|_| |_| |_|\__,_|\__, |\___|_|
                                                |___/

 A tool to assist building OCI seed images for Image Based Upgrades (IBU)

time="2023-11-28 11:50:12" level=info msg="OCI image creation has started"
time="2023-11-28 11:50:12" level=info msg="Creating seed image"

... TRUNCATED ...

time="2023-11-28 11:55:25" level=info msg="OCI image created successfully!"
time="2023-11-28 11:55:25" level=info msg="Cleaning up seed cluster"

... TRUNCATED ...

time="2023-11-28 11:56:37" level=info msg="Seed cluster restored successfully!"
```

Notice that the `--recert-image` flag is optional (mainly used in disconnected environments), if not provided the
tool will use `quay.io/edge-infrastructure/recert:latest` as the default recert image.

> **Note:** For a disconnected environment, first mirror the `ibu-imager` and `recert` container images to your local 
> registry using [skopeo](https://github.com/containers/skopeo) or a similar tool.
