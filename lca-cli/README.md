# LCA CLI

`lca-cli` creates OCI seed images for image-based upgrades and provides commands for
image-based install, post-pivot configuration, and IP configuration. Its `create-usb`
command assembles bootable USB-based IBI media (the live ISO at LBA 0 with the data
partitions appended after it).

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

With `--output`, `create-usb` assembles bootable media following the CNF-27197-proven
DOS/MBR layout: it builds the customized RHCOS live ISO, writes it to the **front of the
image (LBA 0)** so the ISO's own DOS/MBR table and EFI boot entries are preserved, and
appends the formatted, labelled data partitions after it. `--lca-image` is **required** for
assembly. `--write-device` remains deferred: the only target is a sparse `.img`. The
`ibi-images` partition is populated with the OCI image layout, the on-media `digests.json`,
and the staged `lca-cli` binary (self-contained media also carries the read-only durability
`store/`); the `ibi-status` partition ships empty (its runtime records are written on-node).
The `ibi-images` partition is **sized to its measured payload** (staged into scratch, measured,
then set to that payload plus ext4 metadata and free-space headroom, with an 8 GiB minimum), so
an oversized payload fails the `--usb-size` budget at planning time with a clear size error rather
than overrunning a fixed reservation during population. Assembly fails if the resulting
whole-stick size exceeds `--usb-size`. End-to-end boot verification on reference hardware is
tracked by CNF-27158.

The command reads a single YAML or JSON manifest and validates its schema and fields, then
builds the live ISO: `BuildLiveISO` fetches the RHCOS live ISO **over HTTPS and verifies its
sha256 before use** (a non-HTTPS URL, an unverified download, or a checksum mismatch fails the
build), embeds an ignition that auto-runs `lca-cli ibi` on boot, and produces the bootable
customized ISO. The exact shipping `lca-cli` binary is extracted from `--lca-image` and
**staged on p2** (`/run/ibi-images/lca-cli`) rather than embedded in the ISO ignition,
because at ~90 MB it far exceeds the fixed ISO ignition embed area; a label-based `.mount`
unit in the embedded ignition mounts `ibi-images` and the embedded unit execs `lca-cli` from
there. For the media layout rationale (why DOS/MBR, not GPT), the verified-ISO contract and
embedded-ignition details, and the self-contained p3 config tree, digest pinning, and offline
mirror-resource rules, see [docs/create-usb.md](../docs/create-usb.md).

Assembling the media requires a Linux host with loop-device access and `losetup`, `sfdisk`,
`blockdev`, `udevadm`, `mkfs.ext4`, `blkid`, and `lsblk` available (every create-usb partition
is `ext4`). Run with sufficient privileges to create and format loopback partitions.

```bash
./bin/lca-cli create-usb --manifest manifest.yaml --output usb.img --lca-image <lca-cli-pullspec>
```

### Partition layout

The customized live ISO owns the front of the media (its own ISO9660/EFI partitions); the
data partitions below are **appended after it** in the free MBR slots. On-node code discovers
them **by filesystem label**, not by partition number, so the number each one receives is an
implementation detail. Reservation sizes (the sparse `.img` does not consume real bytes until
content is written). `ibi-images` is **sized to its measured payload** — the payload is staged
into scratch first, measured, and the partition is set to that payload plus ext4 metadata and
free-space headroom, rounded up, with an **8 GiB minimum** floor; the other partitions are
fixed-size:

| Role | Filesystem label | FS | Size | Modes | Content today |
| --- | --- | --- | --- | --- | --- |
| live ISO | — (ISO9660/EFI) | — | ISO size | both | The verified, ignition-customized bootable RHCOS live ISO at LBA 0 |
| images | `ibi-images` | ext4 | payload-derived (≥ 8 GiB) | both | OCI image layout + `digests.json`, the staged `lca-cli` binary, and (self-contained) the read-only durability `store/` |
| config | `cluster-config` | ext4 | 256 MiB | self-contained only | Rendered `/opt/openshift` config tree (empty if none supplied) |
| status | `ibi-status` | ext4 | 64 MiB | both | Empty (runtime records written on-node by CNF-27163) |

For **self-contained** media, MBR's four-primary limit (the ISO already uses two) means
`ibi-images` is appended as a primary and `cluster-config` + `ibi-status` are logical
partitions inside an MBR extended container.

### Flags

| Flag | Description |
| --- | --- |
| `--manifest`, `-f` | Path to the USB-based IBI YAML/JSON manifest (required). |
| `--auth-file` | Workstation credentials used at build time to pull `--lca-image` (never written to the media). |
| `--recert-image` | Recert image pull-spec added to the on-media image set and mirrored into the OCI image layout (defaults to `$RELATED_IMAGE_RECERT_IMAGE` or the built-in default). |
| `--lca-image` | lca-cli image pull-spec. The exact shipping `lca-cli` binary is extracted from it and staged on the `ibi-images` partition (not embedded in the ISO); the embedded ignition mounts that partition and execs it on boot. Required for media assembly. |
| `--output` | Path of the `.img` file to write. The path must not already exist — the command refuses to overwrite an existing file. Mutually exclusive with `--write-device`; exactly one is required. |
| `--write-device` | Reserved block-device target. The command rejects it (the only supported target is a sparse `.img`). Mutually exclusive with `--output`; exactly one target is required. |
| `--rhcos-live-iso` | Override the manifest `rhcosLiveIso.url`. Must be set together with `--rhcos-live-iso-sha256` (they override as a pair). |
| `--rhcos-live-iso-sha256` | Override the manifest `rhcosLiveIso.sha256`. Must be set together with `--rhcos-live-iso`. |
| `--usb-size` | Whole-stick size budget (e.g. `14Gi`, `32G`). The build fails if the final apparent media size exceeds it. Empty disables the check. |

### Manifest schema

The manifest has two profiles, selected by the `mode` field:

- `install-only` — installs the seed and shuts down.
- `self-contained` — installs and reconfigures the node from an on-media site configuration, with
  no network access.

Fields common to both profiles (except the optional ISO override):

| Field | Type | Description |
| --- | --- | --- |
| `mode` | string | `install-only` or `self-contained`. |
| `seedImage` | string | The seed OCI image. Must be digest-pinned with a full digest (`repo@sha256:<64-char lowercase hex>`). |
| `seedVersion` | string | The seed image version. |
| `releaseVersion` | string | The OpenShift release version. |
| `installationDisk` | string | The target installation disk (e.g. `/dev/disk/by-id/ata-xxxx`). |
| `disconnected` | bool | Must be `true` for both profiles (it is not a knob). |
| `shutdown` | bool | Must be set explicitly and match the mode: `true` for `install-only`, `false` for `self-contained`. Omitting it fails the build. |
| `rhcosLiveIso.url` | string | Optional HTTPS URL of the RHCOS live ISO. Omit with `sha256` to use the ISO matching the seed. |
| `rhcosLiveIso.sha256` | string | Optional SHA-256 digest of the RHCOS live ISO (64-character lowercase hex). Required with `url`. |

Omit `rhcosLiveIso` unless you need to supply an HTTPS mirror. By default the builder
reads the seed's booted RHCOS build ID from `rpm-ostree.json` and obtains the matching
live ISO and checksum from official build metadata. A manually supplied ISO from a
different RHCOS build can fail during seed extraction or installation because its
SELinux policy may not recognize labels in the seed. If you set one field, set both.

`install-only` adds the following optional field (rejected under `self-contained`):

| Field | Type | Description |
| --- | --- | --- |
| `extraPrecacheImages` | list of string | Additional images to precache. |

`self-contained` adds the following fields (rejected under `install-only`):

| Field | Type | Description |
| --- | --- | --- |
| `clusterManifests` | list of `{path}` | Cluster manifest files to render onto the media (required, non-empty; each entry's `path` must be non-empty). |
| `extraManifests` | list of `{path}` | Additional manifest files to render onto the media (optional; when present, each entry's `path` must be non-empty). |
| `siteConfig` | object | The site configuration (required; see below). |

`siteConfig` (self-contained only; required):

| Field | Type | Description |
| --- | --- | --- |
| `clusterName` | string | Cluster name (required). |
| `baseDomain` | string | Base domain (required). |
| `hostname` | string | Node hostname (required). |
| `nodeIPs` | list of string | Node IP addresses (optional; each entry must be a valid IP). |
| `machineNetworks` | list of string | Machine network CIDRs (optional; each entry must be a valid CIDR). |
| `rawNMStateConfig` | string | NMState configuration YAML, provided as a string. |
| `sshKey` | string | Optional OpenSSH public authorized keys for the live ISO's `core` user and the installed node. Use one key per line. Never provide a private key. |
| `pullSecret` | string | Pull secret, as literal JSON or an `@file` reference (see below). Required. |

The manifest is parsed strictly: unknown fields and duplicate keys are rejected.
Omit `sshKey` when SSH access is not needed; the installer still runs normally.
With network connectivity, a key enables remote monitoring or troubleshooting during USB prepare.

### Pull-secret `@file` references

In a `self-contained` manifest, `siteConfig.pullSecret` accepts either literal JSON or an `@file`
reference that is resolved on the workstation at build time. For example:

```yaml
siteConfig:
  pullSecret: "@/path/to/pull-secret.json"
```

The command reads the referenced file during validation and fails if it is missing or
does not contain valid JSON. The resolved value is rendered into the `SeedReconfiguration`
(`cluster-configuration/manifest.json` on the `cluster-config` partition), so a
self-contained stick carries the pull secret in the clear — treat the media as a
secret-bearing credential (see the threat model in
[docs/create-usb.md](../docs/create-usb.md#threat-model-physical-possession-is-trusted)).

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
clusterManifests:
  - path: manifests/cluster.yaml
siteConfig:
  clusterName: example
  baseDomain: example.com
  hostname: node0
  pullSecret: "@/path/to/pull-secret.json"
```

### Image payload (`digests.json`)

`create-usb --output` populates the `ibi-images` partition with an OCI image layout and
`digests.json`. The `BuildImageLayout` stage resolves the profile's image set to digests,
mirrors it into a build-unique OCI layout directory (named `oci-<unique>`), and emits a
digest manifest. For self-contained profiles it first runs `RenderP3Tree` and scans the
rendered manifests for image references and offline mirror-resource rules. It pins image
references to the mirrored digests after mirroring; see
[docs/create-usb.md](../docs/create-usb.md). The format and publication behavior below
describe that stage.

When `BuildImageLayout` runs, it emits the manifest in two places, byte-identical:

- `digests.json` at the media (p2) root — the committed, read-only on-media reference.
- `<output>.digests.json`, beside the `--output` image — an audit-only, best-effort build-side copy.

Schema (producer emits **version 3**; the on-node consumer still accepts versions 1
and 2, which carry no `sources` / `signatures` respectively):

```json
{
  "version": 3,
  "ociLayoutPath": "oci-1234567890",
  "images": [
    {
      "name": "quay.io/openshift-release-dev/ocp-release",
      "digest": "sha256:9a1f…c3",
      "ociTag": "ocp-release-0",
      "sources": ["quay.io/openshift-release-dev/ocp-release:4.20.0"],
      "signatures": [
        {
          "instanceDigest": "sha256:0b2e…7a",
          "blobs": [{ "sha256": "e3b0…b855", "size": 568 }]
        }
      ]
    }
  ]
}
```

| Field | Type | Description |
| --- | --- | --- |
| `version` | int | Schema version. The image stage emits `3`. Consumers accept `1`, `2`, and `3`. |
| `ociLayoutPath` | string | OCI image layout directory, relative to the media root. Each build mirrors into a fresh, build-unique directory (`oci-<unique>`); the consumer locates the layout through this field. |
| `images[].name` | string | Canonical repository name the image is stored under. |
| `images[].digest` | string | Digest of the top-level `index.json` descriptor to import. For multi-arch content this is the image-index digest; the layout preserves the full multi-arch index so the node imports the instance matching its platform. |
| `images[].ociTag` | string | Layout reference name (`org.opencontainers.image.ref.name`) of the descriptor. Unique per image. |
| `images[].sources` | list of string | Source references (including tags) that requested this image. Version 2+ only; lets disconnected prepare match seed requirements by exact source and register tagged aliases of the imported digest. Empty / omitted in version 1. |
| `images[].signatures` | list | Source signatures keyed by manifest instance digest (top-level manifest/index digest and each signed per-platform instance); each blob records `size`+`sha256`. Version 3 only; bytes live under `<ociLayoutPath>/signatures/`, re-attached on import. Omitted in v1/v2. |

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

The image stage also emits an informational resulting-size estimate (the OCI-layout subtotal and
the image-stage subtotal — the layout plus the on-media `digests.json`). This subtotal is reported
for visibility only; it does not determine the `--usb-size` budget and omits content staged by the
later media-assembly step and the final filesystem/ISO overhead. The budget is enforced separately
by `validateUSBSizeBudget` against the final assembled image's apparent size (`containerSize`: the
ISO plus every data partition, including the measured `ibi-images` payload and assembly/alignment
overhead).

### Image trust and signature preservation

Mirroring runs under the **build host's configured container trust policy**
(`signature.DefaultPolicy` — `$CONTAINERS_POLICY_JSON`, `/etc/containers/policy.json`, …),
not a hardcoded accept-anything policy: a host configured to require signatures rejects an
unsigned or untrusted source, and an unsigned image is mirrored only when the host policy
permits it. If the host has **no** trust policy configured at all, the build logs a loud
warning and falls back to accepting any source (so an unconfigured host still builds); a
malformed policy is a hard error. Digest-pinning additionally guarantees the content is
immutable and content-addressed.

An OCI image layout cannot store an image's signatures, so the signatures the source
registry serves are **captured and staged beside the layout** (under
`<ociLayoutPath>/signatures/`, keyed by manifest instance digest, each blob recorded with
its size and sha256 in `digests.json`). When the node imports an image into
containers-storage — which *can* store signatures — the staged bytes are verified against
their recorded hashes and **re-attached**, so the installed store carries the same
signatures the registry served. A missing, truncated, or hash-mismatched signature **fails
the import** rather than silently dropping trust material.

For a **multi-arch index**, containers-storage stores only the single platform instance the
node imports, so the installed store carries **that platform instance's** signatures
(addressable as the image's default set and under the platform-instance digest) — the
signatures that sign the single manifest the store can verify. **Index-level (manifest-list)
signatures are not restored** into a single-platform store: querying the index digest resolves
to the stored platform image and returns its platform signatures, not a distinct
index-signature set. The index-level bytes are still staged and verified on media, but not
re-attached on a single-platform import. See [`docs/create-usb.md`](../docs/create-usb.md) for
details.
