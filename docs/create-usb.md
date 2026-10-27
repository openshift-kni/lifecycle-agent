# create-usb: self-contained p3 config tree

The create-usb package builds USB-based image-based-install (IBI) media from a single
manifest. Two profiles are supported:

- **install-only** — media that installs the seed and shuts down. It ships no on-media
  site configuration (no p3 config tree).
- **self-contained** — media that installs the seed and then reconfigures the node from an
  on-media site configuration, without contacting the network.

The CLI `lca-cli create-usb --output` path assembles bootable media: it builds the customized
live ISO, writes it to the front of the image, appends the formatted, labelled data partitions
after it (the layout below), and populates the `ibi-images` partition with the OCI image layout,
the on-media `digests.json`, and the staged `lca-cli` binary (self-contained media also carries
the read-only durability `store/`). The p3 render / digest-pin / mirror-scan behavior
(`RenderP3Tree`, run from the `BuildImageLayout` stage) and the OCI image mirroring into
`ibi-images` are wired into the command. See
[lca-cli/README.md](../lca-cli/README.md#creating-usb-media) for the CLI behavior and manifest
schema.

This document describes the self-contained **p3 config tree**: how it is rendered from the
manifest, how image references in the shipped manifests are pinned to digests, and which
mirror resources are rejected. It also describes the **media layout**, the **live ISO** build
block (both apply to both profiles), and the self-contained **p2 durability store** built from
the image closure. (Apart from the media layout and live ISO, install-only media is unaffected
by everything below.)

## Creating USB media

`create-usb` reads one YAML/JSON manifest, validates it, and assembles a bootable `.img` with
`--output`. `--lca-image` is required (its `lca-cli` binary is extracted and staged on the media).
`--write-device` is deferred — the only target today is a sparse `.img` that can be `dd`-ed to a
stick later.

```bash
# install-only media
lca-cli create-usb --manifest manifest.yaml --output usb.img \
  --lca-image <lca-cli-pullspec> --usb-size 14Gi

# self-contained media (same command; the manifest's mode selects the profile)
lca-cli create-usb --manifest manifest.yaml --output usb.img \
  --lca-image <lca-cli-pullspec> --auth-file ./auth.json --usb-size 32Gi
```

Both profiles use the same command; the manifest `mode` selects `install-only` vs
`self-contained`. `--auth-file` supplies workstation registry credentials used at build time only
(to pull `--lca-image` and mirror the image set); it is never written to the media.

### Size budget (`--usb-size`)

`--usb-size` is the whole-stick size budget (e.g. `14Gi`, `32G`). When set, the build fails if the
**final apparent image size** — the live ISO plus every data partition and the MBR
extended-container/alignment overhead — exceeds it, so the resulting `.img` is guaranteed to fit
the target stick. The check validates the final apparent size rather than the staged-byte subtotal
(`SizeReport.StagedBytes`), which deliberately excludes partition/alignment overhead. An empty
`--usb-size` disables the check.

The `ibi-images` partition is **sized to its actual payload**, not a fixed reservation: the OCI
image layout, `digests.json`, the staged `lca-cli` binary, and (self-contained) the durability
`store/` are built into scratch space first, the staged tree is measured, and the partition is set
to the measured payload plus ext4 metadata and free-space headroom, rounded up to a whole MiB, with
an **8 GiB minimum** floor. The `--usb-size` budget is then evaluated against this final plan and
the staged tree is copied onto the formatted partition (with a free-space check). Because the
partition is planned around the measured payload, an oversized payload fails `--usb-size` at
planning time with a clear size error instead of overrunning a fixed partition during population.

## Media layout: the live ISO owns LBA 0

The media follows the layout validated by the CNF-27197 USB-IBI testing-workflow spike and
chosen by the Developer of Record: **the customized RHCOS live ISO owns the front of the media
(LBA 0)**, and the data partitions are appended after it using the ISO's own **DOS/MBR**
partition table.

- The customized ISO is copied **byte-for-byte** to the front of the image, so its own DOS/MBR
  table and the EFI entries that make it bootable under UEFI/Secure Boot stay at sector 0. The
  image is then grown to the full media size.
- The data partitions are appended into the free MBR slots **after** the ISO with
  `sfdisk --append`; the ISO's own partitions are snapshotted first and never touched. MBR
  allows only four primaries and the ISO uses two, so **install-only** appends two primaries
  (`ibi-images`, `ibi-status`) and **self-contained** appends `ibi-images` as a primary plus an
  MBR extended container holding the `cluster-config` and `ibi-status` logical partitions.
- Partitions are discovered on-node **by filesystem label** (`ibi-images`, `cluster-config`,
  `ibi-status`), never by partition number, so the number sfdisk assigns is an implementation
  detail.

Before the image is grown or appended to, the build **inspects the ISO's actual MBR table**
(its occupied primary slots and the first safe append offset) and fails with a clear,
actionable error if the ISO does not leave enough free primary slots for the selected mode —
the layout is verified, not assumed.

GPT was ruled out: a real RHCOS isohybrid live ISO has a hybrid DOS/GPT layout on which
`sgdisk --move-second-header` fails (*"Invalid partition data!"*), so a GPT append path cannot
assemble media. Writing the ISO as raw bytes into a GPT partition offset does not boot either,
because the ISO's protective MBR/ESP never land at LBA 0. The DOS/MBR "append after the ISO"
layout is the one the spike booted under Secure Boot. End-to-end boot verification on reference
hardware is tracked by CNF-27158.

## The live ISO: verified source and embedded prepare

The live ISO at the front of the media is the bootable RHCOS live ISO, customized so the node
runs `lca-cli ibi` automatically on boot. `BuildLiveISO` builds it:

1. **Verified ISO source (HTTPS + sha256).** The RHCOS live ISO is fetched from
   `rhcosLiveIso.url` **over HTTPS only** and its SHA-256 is verified against
   `rhcosLiveIso.sha256` **before the ISO is used**. Any of the following **fails the build**:
   a non-`https` URL (plain HTTP is never used, and a redirect to a non-`https` URL is
   refused), a download error (missing or unreachable source), or a checksum mismatch. The
   ISO host must therefore serve the ISO over HTTPS. The RHCOS live ISO on
   `mirror.openshift.com` is public and needs no pull secret; only the container images
   mirrored into `ibi-images` need `--auth-file`.
2. **Embedded ignition (config + units only).** The build embeds a small ignition config
   (version 3.2.0) that writes into the live environment: the rendered `IBIPrepareConfig` at
   `/etc/lca-cli/ibi-config.json`, a label-based systemd `.mount` unit for the `ibi-images`
   partition (`Where=/var/mnt/ibi-images`, `What=/dev/disk/by-label/ibi-images`), and a oneshot
   systemd unit (`lca-ibi.service`) that runs `lca-cli ibi -f /etc/lca-cli/ibi-config.json` on
   boot, ordered after the `.mount`. The embedded config sets `disconnected: true`,
   `localImagesPath` to the on-node `ibi-images` mount point, and `expectedDigestsPath` to the
   on-media `digests.json`. `shutdown` is carried from the manifest: install-only ends in
   shutdown, self-contained sets `shutdown: false` so `lca-cli` performs the pivot reboot (the
   pivot / pivot-failure logic and its end-to-end verification are owned by CNF-27164).
3. **Exact shipping binary, staged on p2 (not embedded in ignition).** The stock RHCOS live
   ISO ships no `lca-cli`, so the binary is extracted from the `--lca-image` container at build
   time. It is **not** embedded in the ISO ignition: at ~90 MB uncompressed it far exceeds the
   fixed RHCOS ISO ignition embed area (~256 KB) and embedding it would fail the build. Instead
   the extracted binary is delivered on the `ibi-images` partition at
   `/var/mnt/ibi-images/lca-cli`, byte-identical to the binary that lives alongside the
   mirrored images. The `lca-ibi.service` unit's `ExecStart` points at that path and is ordered
   after the label-based `ibi-images` `.mount` unit (via `Requires=`/`After=`), so systemd pulls
   in and waits for that mount before running it. The media-assembly pipeline copies the
   extracted binary onto p2; `BuildLiveISO` extracts and returns it for that step. This keeps
   the ISO ignition tiny regardless of binary size.

   **Mount ownership on boot.** The boot-time `.mount` unit owns the `ibi-images` mount. When the
   disconnected prepare phase (`lca-cli ibi`) runs, it detects that `ibi-images` is already mounted
   read-only from the uniquely label-discovered device — checking the **host** mount namespace, so a
   container-local view cannot give a false answer — and **reuses** that mount, returning a no-op
   cleanup so the systemd-owned mount is never torn down from under the unit. If the mount point is
   instead occupied by a different device, or the expected device is mounted read-write, prepare
   fails before touching the installation disk. When nothing is mounted there (the connected/online
   path, or a manual invocation), prepare mounts the device read-only itself and unmounts it when
   done, as before.

### Executing `lca-cli` from `ibi-images`: exec-capable mount and SELinux context

The `ibi-images` partition is mounted **read-only but without `noexec`**, because the boot-time
`lca-ibi.service` execs the staged `lca-cli` binary directly from it. Dropping `noexec` is
necessary but not sufficient under SELinux **enforcing**: the `ibi-images` ext4 filesystem is
authored on the build host, so its on-disk labels are not the node's policy labels, and the staged
binary would otherwise present the generic mount default (e.g. `unlabeled_t`/`mnt_t`), which the
service domain is not permitted to execute.

The concrete context and policy path is therefore to give the staged binary an **executable
type**. LCA already establishes this pattern for a staged executable:
`ops.prepareSELinuxTar` relabels a copied `tar` to **`install_exec_t`** (`chcon -t
install_exec_t`) before running it. For a read-only mounted filesystem the equivalent is to assign
an executable type to the whole mount via a `context=` mount option — the standard executable type
is **`bin_t`**. The `.mount` unit therefore carries
**`Options=ro,context=system_u:object_r:bin_t:s0`**: a single change that needs no first-boot
relabel and works on the read-only filesystem, so the staged `lca-cli` presents `bin_t` and the
service domain can exec it under enforcing. The end-to-end **enforcing-mode boot verification** of
the IBI flow on reference hardware remains tracked under **CNF-27158** (a static unit-content check
alone cannot prove execution works); a real USB boot has confirmed that, without this context, the
service fails with `status=203/EXEC` because the binary mounts as `unlabeled_t`.

## The self-contained durability store (p2)

For **self-contained** media the build stages a read-only durability store on the `p2`
(`ibi-images`) partition, beside the OCI image layout and `digests.json`. It is the node's
**primary runtime durability artifact**: a `containers/storage` overlay store that the
installed node registers as a **read-only additional image store**, so the digest-pinned
image closure resolves locally with no registry and no network.

```console
<p2 root>/
  oci-<build-id>/      # OCI image layout + signatures/ subtree (BuildImageLayout)
  digests.json         # on-media digest manifest (BuildImageLayout)
  store/               # read-only containers/storage overlay store (this stage)
  storage.conf         # drop-in registering store/ as a read-only additional image store
```

- **`store/`** is built from the **same digest-pinned closure** recorded in `digests.json`:
  the build reads that committed manifest and imports its images, by their canonical
  `name@sha256:...` references, from the OCI layout into the store. The store and the OCI
  layout can therefore never drift. The import runs in-process through the same
  `containers/image` path the on-node consumer (`localimages`) and prepare use — there is no
  external `skopeo` binary and no registry (the proposal's
  `skopeo copy oci:<layout> containers-storage:...` is realized in-process).
- **`storage.conf`** is a rendered drop-in that registers `store/` under the `overlay`
  driver via `additionalimagestores`, which is read-only by definition. It never references a
  registry.

**install-only media emits nothing here** — it ships no on-media site configuration and no
durability store.

### Target architecture

The media is built on a workstation whose architecture may differ from the node the media
installs, and the OCI layout deliberately preserves **every** architecture. The build therefore
selects the **target node's architecture explicitly** when importing into the store, rather than
defaulting to the build host's. A layout that carries no instance for the target architecture
**fails the build**, so a cross-architecture build can never ship a valid-looking store the node
cannot run.

### Crash-safe, recoverable commit

The store and its `storage.conf` are built into **build-unique staging artifacts** and both are
validated before anything committed is touched. They are then committed together by a
**recoverable swap**: the previous `store/` and `storage.conf` are moved aside, the staging
artifacts are renamed into place, and the previous pair is **restored if any step returns an
error**. So an **in-process commit failure** never destroys the last valid store, and `store/`
is always paired with its matching `storage.conf`.

A **crash mid-commit** is a different case: the previous pair is left in a `.store-backup-*`
backup and is **not restored in place**. Instead, the next build garbage-collects that backup and
**rebuilds the store from the OCI layout + `digests.json`** (the source of truth that always
remains on the media). A rebuild that itself fails is surfaced as a **hard error**, so media
assembly must treat an unsuccessful rebuild as **unusable media** rather than shipping a
store-less medium. Stale staging and backup artifacts from a crashed build are likewise
garbage-collected on the next run.

### Overlay compatibility gate (§6.6)

The store is built with the **overlay** graph driver because that is the driver RHCOS/CRI-O
use for the node's persistent container store; a store built with any other driver (or a
silent fallback to `vfs`) could not be consumed there. Requiring overlay at build time is the
build side of the §6.6 compatibility gate: if the build environment cannot produce an overlay
store, the build **fails** rather than shipping an unusable store. After the import, the build
validates that the staged store is in the overlay driver/layer format CRI-O reads (the
`overlay/` graph-driver directory, a non-empty `overlay-images/images.json`, and
`overlay-layers/layers.json`).

### Registry fallback — documented alternative only

If the overlay read-only-additional-image-store model is ever found unworkable, the
**documented alternative** is the registry fallback: emit a digest `ImageDigestMirrorSet`
(canonical → `localhost:5000`, `mirrorSourcePolicy: NeverContactSource`, never an
`ImageTagMirrorSet`) plus a host-level registry service (its own image, a systemd unit, and
an OCI-layout→registry-storage conversion). Because post-pivot runs
`deleteAllOldMirrorResources` before applying the config directory, the IDMS must be in the
applied set, and — critically — the IDMS and the registry service are an **all-or-nothing
unit**: shipping the IDMS without a running `localhost:5000` registry would break image
resolution. This fallback is **not implemented**; it is the deferred §6.6 contingency, and the
rendered `storage.conf` never references a registry. It would only be built if the compat gate
above proves overlay unworkable.

## The /opt/openshift config tree

For self-contained media the build renders the `/opt/openshift` config tree that post-pivot
consumes on the node:

```console
<p3 root>/
  cluster-configuration/
    manifest.json          # SeedReconfiguration built from siteConfig
    manifests/             # clusterManifests sources, expanded
  extra-manifests/         # extraManifests sources, expanded
```

- **`cluster-configuration/manifest.json`** is the `SeedReconfiguration` built from
  `siteConfig` (clusterName, baseDomain, hostname, nodeIPs, machineNetworks,
  rawNMStateConfig, sshKey, pullSecret). `api_version` is set to the only version post-pivot
  accepts, so its version check passes.
- **`cluster-configuration/manifests/`** and **`extra-manifests/`** are rendered from the
  `clusterManifests` / `extraManifests` sources.

The destination must be a **fresh** (absent or empty) directory. The render writes only the
current inputs, and the scan, digest-pin, and mirror-resource passes operate only on the
files it just wrote — so a manifest left over in a reused tree would never be scanned or
pinned, yet post-pivot would still apply it from the media. A nonempty destination therefore
**fails the build** rather than shipping a stale manifest alongside the current ones.

### Deterministic, non-recursive expansion

Each manifest source is a path to a file or a directory:

- A **file** source is copied verbatim.
- A **directory** source expands **non-recursively, in lexical order**: its files are copied
  verbatim in filename order and its sub-directories are **not** descended into.

A **destination-name collision** fails the build: if two sources would write the same
file name into the same destination directory, the build stops rather than silently
overwriting one with the other.

### network-configuration/ is intentionally unpopulated

The current `siteConfig` schema carries network configuration only as `rawNMStateConfig`,
which post-pivot applies with `nmstatectl` straight from `manifest.json`. There is no input
for individual `network-configuration/system-connections/` nmconnection files, so the
`network-configuration/` directory is **not** rendered. This is sufficient for the network
input self-contained media supports today; rendering `system-connections/` would require a
future `siteConfig` schema addition. A post-pivot test
(`TestRenderedManifestSuppliesNMState`) proves the rendered `manifest.json` supplies
`rawNMStateConfig` to the nmstate application path.

## Image references: scan, normalize, and digest-pin

Nothing resolves a shipped `name:tag` reference to a digest offline, so every image
reference the build **discovers** in the shipped manifests is **pinned to a digest**
(`name@sha256:...`) — see *Scope of the pin guarantee* below for exactly what is discovered.
The build:

1. **Scans** the rendered manifests for image references. References are discovered
   known-image-fields-first: a scalar under any field whose key is `image` or ends in
   `image` case-insensitively (`containers[].image`, `CatalogSource.spec.image`,
   `operandImage`, `operatorImage`, `kubeRBACProxyImage`, …) is treated as an image even
   when it is a short, domain-less form such as `busybox:1.32`. Everywhere else a
   parser-validated fallback applies: a scalar is treated as an image only if it has an
   explicit registry domain **and** an explicit tag or digest, so values such as
   `apiVersion: apps/v1` are not mistaken for images.

   **Scope of the pin guarantee.** Because the fallback requires a registry domain, a
   short, domain-less image reference (`busybox:1.32`) hidden in a field whose key does
   **not** end in `image` is **not** auto-discovered and is therefore not pinned. Put such
   references in an `*image`-suffixed field (or write them fully-qualified) so the build
   discovers them. Conversely, any value under an `*image`-suffixed field that is not a
   pullable image has no shipped digest and **fails the build** — the unsupported case is
   explicit, never silent.
2. **Normalizes** every reference to fully-qualified `name:tag` form using the
   `containers/image` reference parser: a bare name (`busybox`) becomes
   `docker.io/library/busybox:latest`, a short tag (`busybox:1.32`) becomes
   `docker.io/library/busybox:1.32`, and an untagged registry path gains `:latest`
   (matching Kubernetes semantics). Already-digested references pass through unchanged.
   The normalized references are unioned into the create-usb image set and mirrored.
3. **Rewrites** every tag-form reference in the shipped manifests to the digest actually
   mirrored for this build. A reference that was **not** mirrored has no offline-pull target
   and **fails the build**.

### Preservation guarantee

The rewrite edits YAML scalar nodes in place. The guarantee is **semantic preservation** —
the rewritten manifest is meaning-equivalent to the source except that tag references are
pinned to digests — **not** byte-for-byte formatting. A node round-trip may change quoting
or indentation outside the edited scalar; comments, anchors/aliases, and multi-document
files are preserved in meaning, which is characterized by
`TestRewriteImageReferences_Preservation`.

An **aliased** image reference (`image: *img`) is followed to its anchor and the anchored
scalar is pinned in place, so the anchor and every alias to it resolve to the digest. A
tag-form reference reachable only through an alias is pinned, not left behind
(`TestRewriteImageReferences_Alias`).

## Mirror resources must be local and offline

The `clusterManifests` / `extraManifests` are applied verbatim by post-pivot, so a mirror
resource that could reach the network would break the disconnected guarantee. The build
**fails** on any of the following resource types unless it is strictly local and offline:

- **`ImageDigestMirrorSet`** and **`ImageTagMirrorSet`** (`config.openshift.io/v1`): every
  mirror entry must set `mirrorSourcePolicy: NeverContactSource`, and every mirror's host
  must be the on-node local registry `localhost:5000`. A repository path after the host is
  allowed (e.g. `localhost:5000/openshift/release`); only the host is checked.
- **`ImageContentSourcePolicy`** (`operator.openshift.io/v1alpha1`): **rejected outright.**
  ICSP has no `mirrorSourcePolicy` field and always falls back to contacting the source, so
  it can never satisfy the offline rule. This is a deliberate strict policy for
  self-contained media.

The primary durability model ships no mirror resource at all, so any mirror resource is
already suspect; these rules bound what is permitted when one is present.

A mirror resource wrapped in a `List` (`items[]`) is unwrapped and scanned as well, matching
how post-pivot applies each list member verbatim, so a resource cannot bypass these rules by
being nested in a `List`.

## The ibi-status (p4) partition and per-phase results

Both profiles' media carry a small **writable** partition labelled `ibi-status` (the "p4"
partition), formatted `ext4`. It is created, formatted, and labelled empty by create-usb
(see CNF-28670); the prepare phase consumes it to record a machine-readable per-phase result
that a technician can read off-box without a serial console.

**On-partition layout.** The machine-readable result files are written at the **root** of
p4; the compact diagnostic log bundle is written under a `logs/` directory, keeping results
and human diagnostics separated:

```console
<p4 root>/
  prepare.json             # prepare phase result (both success and failure)
  logs/
    prepare-logs.txt.gz    # best-effort, size-capped journal capture
```

`prepare.json` is written by the prepare phase on **both** the success and failure paths,
using an atomic, durable sequence (temp file → `fsync` → `rename` → directory `fsync` →
whole-filesystem `sync`) **before** the terminal action, so the record survives a power loss
rather than being lost in the buffer cache. Its schema:

```json
{
  "phase": "prepare",
  "profile": "install-only",
  "result": "success",
  "timestamp": "2026-09-02T12:00:00Z",
  "seedVersion": "4.18.3",
  "checks": { "imagesExpected": 188, "imagesPresent": 188 },
  "error": null,
  "reentry": "none"
}
```

`ext4` is required: the atomic write contract above only holds on a journaled filesystem, so
create-usb rejects `vfat` for this partition at build time.

### Mandatory p4 and the re-entry guard

Before the prepare phase performs any destructive disk operation (disk cleanup /
`coreos-installer install`), it enforces a hard precondition in **both** profiles: there must
be **exactly one** writable `ibi-status` device. Zero or more than one device, a mount
failure, a read-only partition, or an unreadable/malformed `prepare.json` all **fail closed**
— the node halts powered-on without wiping the installation disk.

If a `prepare.json` with `result: success` is already present, the current boot is a
**re-entry over a completed install**. The prepare phase refuses to wipe: it preserves the
existing `result: success` (it never rewrites it), records `reentry: blocked`, and halts the
node powered-on. Repeated re-entries remain safe — the completed-install marker is never
destroyed, so a second destructive prepare can never re-wipe a finished install.

### Terminal action: power-off vs halt powered-on

- On **success**, the node powers off (install-only). This power-off is the install-only
  success signal; the self-contained success/reboot-pivot split is handled separately.
- On **failure**, the node **halts powered-on** (it isolates the emergency target) rather
  than powering off, so the power-off success signal stays unambiguous and the node remains
  reachable for a technician. A successful prepare whose result record could not be committed
  also halts powered-on rather than signalling success via power-off.

## Image trust and signature preservation

Two independent trust mechanisms apply to the mirrored image set: the **build-time trust
policy** that governs what may be mirrored, and the **preserved registry signatures** that
let the installed node verify the images it runs.

### Build-time trust policy

Mirroring runs under the build host's **configured container trust policy**
(`signature.DefaultPolicy` — `sysCtx.SignaturePolicyPath`, `$CONTAINERS_POLICY_JSON`, or
`/etc/containers/policy.json`), not a hardcoded accept-anything policy. A host configured to
require signatures for the source registries therefore rejects an unsigned or untrusted
source, and an unsigned image is mirrored only when the host policy permits it. If the host
has **no** trust policy configured at all, the build cannot enforce publisher trust: rather
than fail every build on an unconfigured host, it logs a loud warning and falls back to
accepting any source. A malformed/unreadable policy is a hard error. Digest-pinning is a
separate, always-on guarantee of content-addressing and immutability — not publisher
identity — so the two are complementary.

### Preserved registry signatures

An OCI image layout **cannot store an image's signatures** (`copy.Image` into an OCI layout
fails outright on a signed source). So the build strips signatures from the layout copy and
**captures them from the source registry out-of-band**, staging the raw bytes beside the
layout:

```console
<ociLayoutPath>/signatures/<algo>/<encoded>/<n>   # the n-th signature of instance <algo>:<encoded>
```

Signatures are captured for the **top-level manifest/index digest and every signed per-platform
instance** (a single-arch image carries a manifest rather than an index; OpenShift release
content is typically signed per instance). Each blob's size
and sha256 are recorded in `digests.json` (`images[].signatures`, schema version 3), the
bundle is read back and verified at build time, and because it lives inside the build-unique
layout directory it is sized, staged, committed, and garbage-collected atomically with the
layout.

On the node, disconnected prepare imports each image into **containers-storage, which *can*
store signatures**. The staged bytes are verified against their recorded size and sha256 and
**re-attached to the image as it is imported** (the import copy carries them through the
containers/image copy pipeline), so the installed store holds exactly the signatures the
registry served. A missing, truncated, or hash-mismatched signature **fails the import
before any terminal action** rather than silently dropping trust material. The import copy
itself reads from the local, physically-trusted media (see the threat model below), so it
preserves the signatures rather than re-verifying them against the local OCI-layout
reference; cryptographic verification against the node's policy happens at container runtime
against the restored signatures.

> **Limitation — multi-arch index signatures.** containers-storage stores **single-platform**
> images. When the source is a multi-arch index, disconnected prepare materialises only the
> instance matching the node's architecture (`copy.Image` with `CopySystemImage`), and the
> store holds **that platform instance's** signatures — addressable both as the image's default
> set and under the platform-instance digest. This is the correct, verifiable representation: the
> stored signatures sign the single manifest the store can actually verify, exactly as a registry
> pull of one platform would produce. The **index-level (manifest-list) signatures are not
> restored** into a single-platform store — querying the index digest resolves to the stored
> platform image and returns its platform signatures, not a distinct index-signature set. The
> capture side still stages the index-level signatures on media (and verifies them at build time)
> for completeness and forward compatibility, but they are not re-attached on a single-platform
> import. Index-signature preservation would require storing the full manifest list as a
> verifiable image, which containers-storage does not do.

## Threat model: physical possession is trusted

USB-based IBI media is a **physical-possession-trusted** artifact: whoever holds the stick can
read everything on it. The data partitions are **unencrypted**, so anyone with the media can read:

- the on-media OCI image layout and `digests.json` (`ibi-images`), and the read-only durability
  `store/` on self-contained media;
- the **self-contained `cluster-config` tree**, which contains the site configuration — including
  the **pull secret** it ships for the node. On self-contained media the pull secret is therefore
  present on the stick in the clear.

Protect the media accordingly: treat a self-contained stick as a secret-bearing credential, build
it on a trusted workstation, hand it off over a trusted channel, and destroy or wipe it when the
install is done. install-only media carries no site configuration or pull secret, so its exposure
is limited to the mirrored image set.

## Media signing (deferred)

Requirement-driven, verifiable **media signing** (so a node can cryptographically verify the media
it booted before trusting its contents) is **not implemented in this story**. The ISO source is
fetched over HTTPS and verified by SHA-256 at build time, and the on-media image set is
digest-pinned, but there is no end-to-end signature over the assembled media itself. Signing is
tracked as deferred follow-up work; until it lands, media integrity rests on the
physical-possession trust model above and the build-time digest/sha256 verification.
