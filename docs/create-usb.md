# create-usb: self-contained p3 config tree

`lca-cli create-usb` builds USB-based image-based-install (IBI) media from a single
manifest. Two profiles are supported:

- **install-only** — media that installs the seed and shuts down. It ships no on-media
  site configuration (no p3 config tree).
- **self-contained** — media that installs the seed and then reconfigures the node from an
  on-media site configuration, without contacting the network.

This document describes the self-contained **p3 config tree**: how it is rendered from the
manifest, how image references in the shipped manifests are pinned to digests, and which
mirror resources are rejected. (install-only media is unaffected by everything below.)

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
