# LCA CLI

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

The purpose of the `lca-cli` tool is to assist in the creation of IBU seed images, which are used later on by
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
-> make cli-build
go mod tidy
Running go fmt
go fmt ./...
Running go vet
go vet ./...
go build -o bin/lca-cli main/lca-cli/main.go
```

> **Note:** The binary can be found in `./bin/lca-cli`.

### Running the tool's help

To see the tool's help on your local host, run the following command:

```shell
-> ./bin/lca-cli -h
lca-cli assists LCA in Image Based Install (IBI), Image Based Upgrade (IBU) and IP Configuration (IPC) workflows.

  Find more information at: https://github.com/openshift-kni/lifecycle-agent/blob/main/lca-cli/README.md

Usage:
  lca-cli [command]

Available Commands:
  completion          Generate the autocompletion script for the specified shell
  create              Create OCI image and push it to a container registry.
  help                Help about any command
  ibi                 prepare ibi
  ibuPrecacheWorkload Start precache during IBU
  ibuStaterootSetup   Setup a new stateroot during IBU
  init-monitor        LCA Init Monitor
  ip-config           IP configuration commands
  post-pivot          post pivot configuration
  restore             Restore seed cluster configurations

Flags:
  -h, --help       help for lca-cli
  -c, --no-color   Control colored output
  -v, --verbose    Display verbose logs

Use "lca-cli [command] --help" for more information about a command.
```

### Running as a container

To create an IBU seed image out of your Single Node OpenShift (SNO), run the following command directly on the node:

```shell
-> export LCA_IMAGE=$(oc get deployment -n openshift-lifecycle-agent lifecycle-agent-controller-manager -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')

-> export AUTHFILE=/path/to/pull-secret.json
-> export SEED_IMG_REFSPEC=quay.io/${MY_REPO_ID}/${MY_REPO}:${MY_TAG}
-> export IMG_RECERT_TOOL=quay.io/edge-infrastructure/recert:v0

-> podman run --privileged --pid=host --rm --net=host \
    -v /etc:/etc \
    -v /var:/var \
    -v /var/run:/var/run \
    -v /run/systemd/journal/socket:/run/systemd/journal/socket \
    -v ${AUTHFILE}:${AUTHFILE} \
    --entrypoint lca-cli ${LCA_IMAGE} create --authfile ${AUTHFILE} \
                                                 --image ${SEED_IMG_REFSPEC} \
                                                 --recert-image ${IMG_RECERT_TOOL}

lca-cli assists LCA in Image Based Install (IBI), Image Based Upgrade (IBU) and IP Configuration (IPC) workflows.

  Find more information at: https://github.com/openshift-kni/lifecycle-agent/blob/main/lca-cli/README.md

time="2023-11-28 11:50:12" level=info msg="OCI image creation has started"
time="2023-11-28 11:50:12" level=info msg="Creating seed image"

... TRUNCATED ...

time="2023-11-28 11:55:25" level=info msg="OCI image created successfully!"
time="2023-11-28 11:55:25" level=info msg="Cleaning up seed cluster"

... TRUNCATED ...

time="2023-11-28 11:56:37" level=info msg="Seed cluster restored successfully!"
```

Notice that the `--recert-image` flag is optional (mainly used in disconnected environments), if not provided the
tool will use `quay.io/edge-infrastructure/recert:v0` as the default recert image.

> **Note:** For a disconnected environment, first mirror the `lca-cli` and `recert` container images to your local
> registry using [skopeo](https://github.com/containers/skopeo) or a similar tool.

## Creating USB media

> **Note:** This section documents the `create-usb` manifest schema and flags. It is the first
> part of the USB-based Image-Based Install (IBI) documentation; the full end-to-end usage guide
> is completed in a follow-up story. Media assembly itself is not yet implemented — the command
> currently parses and validates the manifest and then exits with a not-implemented error.

The `create-usb` subcommand builds self-installing USB media for the USB-based IBI workflow from a
single YAML (or JSON) manifest. It runs on a workstation and validates every build-time invariant
before producing media, so a malformed or inconsistent manifest fails the build rather than
producing unusable media.

```shell
-> ./bin/lca-cli create-usb --manifest manifest.yaml --output usb.img
```

### Flags

| Flag | Description |
| --- | --- |
| `--manifest`, `-f` | Path to the USB-based IBI YAML/JSON manifest (required). |
| `--auth-file` | Path to workstation mirror-registry credentials used at build time only. These are never written to the media. |
| `--output` | Path of the `.img` file to write. Mutually exclusive with `--write-device`; exactly one is required. |
| `--write-device` | Block device to write the media to. Mutually exclusive with `--output`; exactly one is required. |
| `--rhcos-live-iso` | Override the manifest `rhcosLiveIso.url`. Must be set together with `--rhcos-live-iso-sha256` (they override as a pair). |
| `--rhcos-live-iso-sha256` | Override the manifest `rhcosLiveIso.sha256`. Must be set together with `--rhcos-live-iso`. |

### Manifest schema

The manifest has two profiles, selected by the `mode` field:

- `install-only` — installs the seed and shuts down.
- `self-contained` — installs and reconfigures the node from an on-media site configuration, with
  no network access.

Fields common to both profiles (all required):

| Field | Type | Description |
| --- | --- | --- |
| `mode` | string | `install-only` or `self-contained`. |
| `seedImage` | string | The seed OCI image. Must be digest-pinned with a full digest (`repo@sha256:<64-char lowercase hex>`). |
| `seedVersion` | string | The seed image version. |
| `releaseVersion` | string | The OpenShift release version. |
| `installationDisk` | string | The target installation disk (e.g. `/dev/disk/by-id/ata-xxxx`). |
| `disconnected` | bool | Must be `true` for both profiles (it is not a knob). |
| `shutdown` | bool | Must match the mode: `true` for `install-only`, `false` for `self-contained`. |
| `rhcosLiveIso.url` | string | HTTPS URL of the RHCOS live ISO. |
| `rhcosLiveIso.sha256` | string | SHA-256 digest of the RHCOS live ISO (64-character lowercase hex). |

`install-only` adds the following optional field (rejected under `self-contained`):

| Field | Type | Description |
| --- | --- | --- |
| `extraPrecacheImages` | list of string | Additional images to precache. |

`self-contained` adds the following fields (rejected under `install-only`):

| Field | Type | Description |
| --- | --- | --- |
| `clusterManifests` | list of `{path}` | Cluster manifest files to render onto the media (required). |
| `extraManifests` | list of `{path}` | Additional manifest files to render onto the media. |
| `siteConfig` | object | The site configuration (see below). |

`siteConfig` (self-contained only):

| Field | Type | Description |
| --- | --- | --- |
| `clusterName` | string | Cluster name (required). |
| `baseDomain` | string | Base domain (required). |
| `hostname` | string | Node hostname (required). |
| `nodeIPs` | list of string | Node IP addresses. |
| `machineNetworks` | list of string | Machine network subnets. |
| `rawNMStateConfig` | string | NMState configuration YAML, provided as a string. |
| `sshKey` | string | Public SSH key for node access. |
| `pullSecret` | string | Pull secret, as literal JSON or an `@file` reference (see below). Required. |

The manifest is parsed strictly: unknown fields and duplicate keys are rejected.

### Pull-secret `@file` references

In a `self-contained` manifest, `siteConfig.pullSecret` accepts either literal JSON or an `@file`
reference that is resolved on the workstation at build time. For example:

```yaml
siteConfig:
  pullSecret: "@/path/to/pull-secret.json"
```

The referenced file's contents replace the value during the build; the `@` prefix never appears in
the rendered output. The build fails if the file is missing or its contents are not valid JSON.

### Example: install-only

```yaml
mode: install-only
seedImage: quay.io/example/seed@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
seedVersion: 4.20.0
releaseVersion: 4.20.0
installationDisk: /dev/disk/by-id/ata-example
disconnected: true
shutdown: true
rhcosLiveIso:
  url: https://mirror.openshift.com/pub/openshift-v4/amd64/dependencies/rhcos/latest/rhcos-live.x86_64.iso
  sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
extraPrecacheImages:
  - quay.io/example/extra@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
```

### Example: self-contained

```yaml
mode: self-contained
seedImage: quay.io/example/seed@sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
seedVersion: 4.20.0
releaseVersion: 4.20.0
installationDisk: /dev/disk/by-id/ata-example
disconnected: true
shutdown: false
rhcosLiveIso:
  url: https://mirror.openshift.com/pub/openshift-v4/amd64/dependencies/rhcos/latest/rhcos-live.x86_64.iso
  sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
clusterManifests:
  - path: manifests/cluster.yaml
siteConfig:
  clusterName: example
  baseDomain: example.com
  hostname: node0
  pullSecret: "@/path/to/pull-secret.json"
```
