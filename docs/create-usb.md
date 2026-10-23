# create-usb: self-contained p3 config tree

The create-usb package builds USB-based image-based-install (IBI) media from a single
manifest. Two profiles are supported:

- **install-only** — media that installs the seed and shuts down. It ships no on-media
  site configuration (no p3 config tree).
- **self-contained** — media that installs the seed and then reconfigures the node from an
  on-media site configuration, without contacting the network.

Today the CLI `lca-cli create-usb --output` path only scaffolds the GPT placeholder image
(empty formatted partitions). The p1 live-ISO build (`BuildLiveISO`) and the p3 render /
digest-pin / mirror-scan behavior below (`RenderP3Tree`, run from the separate
`BuildImageLayout` stage) are implemented as standalone building blocks; they are not yet
wired into the CLI (CNF-27171 wires them). See
[lca-cli/README.md](../lca-cli/README.md#creating-usb-media) for the current CLI behavior and
manifest schema.

This document describes the self-contained **p3 config tree**: how it is rendered from the
manifest, how image references in the shipped manifests are pinned to digests, and which
mirror resources are rejected. It also describes the **p1 live ISO** build block, which
applies to both profiles, and the self-contained **p2 durability store** built from the
image closure. (Apart from the p1 live ISO, install-only media is unaffected by everything
below.)

## The p1 live ISO: verified source and embedded prepare

The p1 partition carries the bootable RHCOS live ISO, customized so the node runs
`lca-cli ibi` automatically on boot. `BuildLiveISO` builds it as a standalone block (like
`BuildImageLayout` and `RenderP3Tree`, it is wired into the CLI by CNF-27171):

1. **Verified ISO source (HTTPS + sha256).** The RHCOS live ISO is fetched from
   `rhcosLiveIso.url` **over HTTPS only** and its SHA-256 is verified against
   `rhcosLiveIso.sha256` **before the ISO is used**. Any of the following **fails the build**:
   a non-`https` URL (plain HTTP is never used, and a redirect to a non-`https` URL is
   refused), a download error (missing or unreachable source), or a checksum mismatch. The
   ISO host must therefore serve the ISO over HTTPS. The RHCOS live ISO on
   `mirror.openshift.com` is public and needs no pull secret; only the container images
   mirrored into p2 need `--auth-file`.
2. **Embedded ignition (config + unit only).** The build embeds a small ignition config
   (version 3.2.0) that writes two things into the live environment: the rendered
   `IBIPrepareConfig` at `/etc/lca-cli/ibi-config.json`, and a oneshot systemd unit
   (`lca-ibi.service`) that runs `lca-cli ibi -f /etc/lca-cli/ibi-config.json` on boot. The
   embedded config sets `disconnected: true`, `localImagesPath` to the on-node p2
   (`ibi-images`) mount point, and `expectedDigestsPath` to the on-media `digests.json`.
   `shutdown` is carried from the manifest: install-only ends in shutdown, self-contained sets
   `shutdown: false` so `lca-cli` performs the pivot reboot (the pivot / pivot-failure logic
   and its end-to-end verification are owned by CNF-27164).
3. **Exact shipping binary, staged on p2 (not embedded in ignition).** The stock RHCOS live
   ISO ships no `lca-cli`, so the binary is extracted from the `--lca-image` container at build
   time. It is **not** embedded in the ISO ignition: at ~90 MB uncompressed it far exceeds the
   fixed RHCOS ISO ignition embed area (~256 KB) and embedding it would fail the build. Instead
   the extracted binary is delivered on the p2 (`ibi-images`) partition at
   `/var/mnt/ibi-images/lca-cli`, byte-identical to the binary that lives alongside the
   mirrored images. The `lca-ibi.service` unit's `ExecStart` points at that path and declares
   `RequiresMountsFor=/var/mnt/ibi-images`, so systemd waits for the p2 mount before running
   it. The media-assembly wiring (CNF-27171) is what copies the extracted binary onto p2;
   `BuildLiveISO` extracts and returns it for that step. This keeps the ISO ignition tiny
   regardless of binary size.

The customized ISO is written into the reserved p1 region with a **reservation-size guard**:
if the customized ISO is larger than the p1 reservation, the build fails loudly rather than
truncating the ISO.

## The self-contained durability store (p2)

For **self-contained** media the build stages a read-only durability store on the `p2`
(`ibi-images`) partition, beside the OCI image layout and `digests.json`. It is the node's
**primary runtime durability artifact**: a `containers/storage` overlay store that the
installed node registers as a **read-only additional image store**, so the digest-pinned
image closure resolves locally with no registry and no network.

```console
<p2 root>/
  oci-<build-id>/      # OCI image layout (BuildImageLayout)
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
