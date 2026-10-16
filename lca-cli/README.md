# LCA CLI

`lca-cli` creates OCI seed images for image-based upgrades and provides commands for
image-based install, post-pivot configuration, and IP configuration. Its `create-usb`
command currently creates a partitioned placeholder image for USB-based installation.

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

The `create` command produces IBU seed images that other lifecycle-agent components
consume during image-based upgrades.

In that direction, the tool does the following at a high level:

- Saves a list of container images used by `crio` (needed for pre-caching operations afterward)
- Creates a backup of the main platform configurations (e.g., `/var` and `/etc` directories, ostree artifacts, etc.)
- Creates a backup of the ostree repository
- Creates a seed container image (OCI) with all the generated content and pushes it to a remote container registry
  (used during the image-based upgrade workflow afterward)

### Building

Build the binary from the repository root with the Go version required by `go.mod`:

```bash
make cli-build
```

The binary is written to `./bin/lca-cli`.

### Running the tool's help

Use the CLI help for the current command list and flags:

```bash
./bin/lca-cli --help
./bin/lca-cli create-usb --help
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

With `--output`, `create-usb` creates a sparse `.img` containing the planned GPT
partition slots. p1 reserves space for the live ISO; p2 and p4 are empty ext4
filesystems, and self-contained mode also gets an empty ext4 p3. The command does
not download the ISO, populate images or site configuration, or verify bootability.
**The output is not bootable installation media.** `--write-device` remains deferred.

The command reads a single YAML or JSON manifest and validates its schema and fields
before creating the placeholder image. The image mirroring and p3 rendering stages
described below are implemented as separate building blocks but are not yet called
by `create-usb`.

Creating the partition table requires a Linux host with loop-device access and
`losetup`, `sgdisk`, `partprobe`, `udevadm`, and `mkfs.ext4` available. Run with
sufficient privileges to create and format loopback partitions.

```bash
./bin/lca-cli create-usb --manifest manifest.yaml --output usb.img
```

### Flags

| Flag | Description |
| --- | --- |
| `--manifest`, `-f` | Path to the USB-based IBI YAML/JSON manifest (required). |
| `--auth-file` | Reserved for workstation mirror-registry credentials. Explicit use fails until image mirroring is wired into the command. |
| `--recert-image` | Reserved for the recert image in the on-media image set. Explicit use currently fails. |
| `--lca-image` | Reserved for the lca-cli image in the on-media image set. Explicit use currently fails. |
| `--output` | Path of the `.img` file to write. The path must not already exist — the command refuses to overwrite an existing file. Mutually exclusive with `--write-device`; exactly one is required. |
| `--write-device` | Reserved block-device target. The command rejects it while media assembly is incomplete. Mutually exclusive with `--output`; exactly one target is required. |
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
| `shutdown` | bool | Must be set explicitly and match the mode: `true` for `install-only`, `false` for `self-contained`. Omitting it fails the build. |
| `rhcosLiveIso.url` | string | HTTPS URL of the RHCOS live ISO. |
| `rhcosLiveIso.sha256` | string | SHA-256 digest of the RHCOS live ISO (64-character lowercase hex). |

`install-only` adds the following optional field (rejected under `self-contained`):

| Field | Type | Description |
| --- | --- | --- |
| `extraPrecacheImages` | list of string | Additional images to precache. |

`self-contained` adds the following fields (rejected under `install-only`):

| Field | Type | Description |
| --- | --- | --- |
| `clusterManifests` | list of `{path}` | Cluster manifest files to render onto the media (required; each entry's `path` must be non-empty). |
| `extraManifests` | list of `{path}` | Additional manifest files to render onto the media (each entry's `path` must be non-empty). |
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

The command reads the referenced file during validation and fails if it is missing or
does not contain valid JSON. A future media-population stage will use the resolved
value; the current placeholder image contains no pull secret.

### Example: install-only

The image names, URLs, paths, and SHA-256 values in these examples illustrate the
manifest format; replace them with values for your environment.

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

### Planned image payload (`digests.json`)

`create-usb --output` does **not** currently create `digests.json` or an OCI image
layout. The separately implemented `BuildImageLayout` stage resolves the profile's
image set to digests, mirrors it into a build-unique OCI layout directory (named
`oci-<unique>`), and emits a digest manifest. Wiring that stage into the placeholder
image is follow-up work. The format and publication behavior below describe that
standalone stage, not the current `create-usb` output.

When `BuildImageLayout` runs, it emits the manifest in two places, byte-identical:

- `digests.json` at the media (p2) root — the committed, read-only on-media reference.
- `<output>.digests.json`, beside the `--output` image — an audit-only, best-effort build-side copy.

Schema:

```json
{
  "version": 1,
  "ociLayoutPath": "oci-1234567890",
  "images": [
    {
      "name": "quay.io/openshift-release-dev/ocp-release",
      "digest": "sha256:9a1f…c3",
      "ociTag": "ocp-release-0"
    }
  ]
}
```

| Field | Type | Description |
| --- | --- | --- |
| `version` | int | Schema version. Only `1` is supported. |
| `ociLayoutPath` | string | OCI image layout directory, relative to the media root. Each build mirrors into a fresh, build-unique directory (`oci-<unique>`); the consumer locates the layout through this field. |
| `images[].name` | string | Canonical repository name the image is stored under. |
| `images[].digest` | string | Digest of the top-level `index.json` descriptor to import. For multi-arch content this is the image-index digest; the layout preserves the full multi-arch index so the node imports the instance matching its platform. |
| `images[].ociTag` | string | Layout reference name (`org.opencontainers.image.ref.name`) of the descriptor. Unique per image. |

**Crash-safe publication.** The on-media `digests.json` is the single commit point. Each build
mirrors into a fresh, build-unique layout directory (never the one the live manifest references)
and records that directory in `ociLayoutPath`. Because the consumer locates the layout *through*
the manifest, atomically replacing `digests.json` with a single rename is what switches the media
from the previous layout to the new one: before the rename the committed manifest still points at
the intact previous layout, after it the committed manifest points at the fully-written new layout.
A crash or power loss at any point therefore leaves the media consistent — the committed manifest
always points at a complete, matching layout, never a mismatch — and at worst leaves an orphaned
layout directory. Superseded and crash-orphaned layout directories are garbage-collected only
*after* the manifest commit succeeds, so the only intact layout is never deleted. The
`<output>.digests.json` copy is audit-only and best-effort: it is written after the commit, never
participates in it (it may sit on a different filesystem), and a failure to write it does not fail
the build.

The guarantee holds across power loss, not merely process termination: the completed layout (its
files and directories) is fsynced before the commit, the media-root directory entry that names the
new layout directory is fsynced before the manifest can reference it, and the commit rename is
itself made durable by fsyncing the manifest file and the media-root directory, so a power loss can
never persist the manifest rename ahead of the layout data (or the layout's own directory entry) it
references. If the commit rename succeeds but its durability fsync then fails, the medium is treated
as committed — the manifest already points at the complete new layout, so the layout is kept, never
deleted — and the fsync error is surfaced so the caller learns durability was not confirmed. Garbage
collection runs only after the commit is durable, and it re-reads the committed manifest to learn
which layout is live rather than trusting the just-written name. Builds against the same media root
are serialized by an advisory file lock, so concurrent builds cannot interleave their commit/GC
steps and collect a layout the other build just made live.

The build also emits a resulting-size estimate (the OCI-layout subtotal and the image-stage
subtotal — the layout plus the on-media `digests.json`) for the USB-size budget; the estimate
excludes content staged by the later media-assembly step and the final filesystem/ISO overhead.

> **Trust caveat:** mirroring uses an accept-anything signature policy. Pinning each
> reference to a digest guarantees the content is immutable and content-addressed, but it
> verifies **neither publisher identity nor image trust** (no signature verification is
> performed).
