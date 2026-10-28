# Disconnected IBI: self-contained image durability (prepare side)

For **self-contained** USB-based IBI media, the installed node must recover its container
images with **no registry and no network**: images evicted by kubelet garbage collection or
removed by CRI-O's reboot-time cleanup have to come back from a local, durable source. This
document describes the **prepare side** of that durability model — how `lca-cli` prepare
persists the on-media durability store onto the installed disk and registers it as a read-only
additional image store.

The **producer side** (how `create-usb` builds and stages the store on the USB `p2`
partition) is documented in [create-usb.md](create-usb.md#the-self-contained-durability-store-p2).

## Where the store comes from

`create-usb` stages a read-only `containers/storage` **overlay** store on the `p2`
(`ibi-images`) partition for self-contained media, beside the OCI image layout and
`digests.json`:

```console
<p2 root>/
  oci-<build-id>/      # OCI image layout + signatures/ subtree
  digests.json         # on-media digest manifest (v2+)
  store/               # read-only containers/storage overlay store  <-- this is what prepare copies
  storage.conf         # the media's own drop-in (registers store/ at the p2 path)
```

The store is built from the **same digest-pinned closure** recorded in `digests.json`, so it
resolves every image by its canonical `name@sha256:...` reference. Because the importer that
builds it re-attaches the offline image signatures into the store's overlay metadata, the
signatures travel **inside** `store/` — a faithful copy is required to preserve them.

**install-only media carries no durability store**, and the connected flow sources images from
the network; prepare's read-only-store handoff runs for **self-contained media only**.

## What prepare does

The handoff runs inside the disconnected prepare flow, **while the installed stateroot is still
mounted and before the final `sync` and the `p2` unmount** — so the registered additional image
store never points at a path that has already gone away. It is gated on
`Disconnected && Profile == self-contained`; install-only and the connected flow are a clean
no-op.

Steps (all-or-nothing):

1. **Fail closed.** If self-contained media is expected but the pre-built `store/`
   (`/var/mnt/ibi-images/store`) is missing or is not a valid overlay store, prepare **fails**.
   An incomplete recovery source is worse than none, so prepare never silently degrades to an
   install-only-style no-op.
2. **Copy** `store/` onto the installed disk into a staging directory, using a
   metadata-preserving archive copy (the same `go.podman.io/storage/pkg/archive` copier
   `create-usb` uses to stage the store onto `p2`). It preserves symlinks (the overlay link
   farm), hardlinks, numeric ownership, xattrs and special mode bits — **no blanket mode
   rewrite** — so the embedded image signatures and overlay metadata survive. The store stays
   `root:root` and is read-only by additional-image-store semantics plus its ownership, not by
   `chmod`-ing content.
3. **Validate** the copied store: the overlay driver/layer format (the prepare-side half of the
   §6.6 compatibility gate, re-checked on the target node) and completeness against
   `digests.json` — every expected `name@digest` must resolve when the store is opened as a
   read-only additional image store.
4. **SELinux at prepare.** The copy carries the media's mount-option `bin_t` context, which is a
   read-the-media label, not a container-storage one. Prepare relabels the store to the
   container-storage read-only content type (`container_ro_file_t`) **during prepare** (not a
   post-boot-only `restorecon`), so CRI-O can read it the moment offline recovery is needed. The
   label is verified after pivot by the e2e recovery test.
5. **Atomically seal.** The verified, labelled store is moved into place at the post-pivot path
   **`/var/lib/containers/ro-store`** (`stage ⇒ verify ⇒ move`), outside CRI-O's writable
   graphroot. An interrupted copy leaves no partial `ro-store`.
6. **Register.** Only after the store is in place, prepare writes a
   `/etc/containers/storage.conf.d/50-lca-ro-store.conf` drop-in onto the installed deployment
   that adds `/var/lib/containers/ro-store` to `additionalimagestores`:

   ```toml
   [storage.options]
   additionalimagestores = ["/var/lib/containers/ro-store", {append = true}]
   ```

   `append = true` is required: the vendored `go.podman.io/storage` `conf.d` loader otherwise
   has a drop-in array **replace** `additionalimagestores`, which would drop any entries an
   existing `storage.conf` already sets. `append` **merges** into the effective config,
   preserving existing entries. If the drop-in write fails, the sealed store is rolled back so
   the store and its registration stay atomic as a pair.

Post-boot, CRI-O resolves every closure image from `ro-store` by canonical `name@digest` with
no registry and no IDMS. The copy survives both kubelet GC and CRI-O's reboot-time corruption
cleanup, because both act only on the writable graphroot. There is **no systemd service**.

## Disk-space trade-off

The image closure ships on the USB **twice** for self-contained media — the OCI layout (which
prepare's live/stateroot import consumes) **plus** the pre-built `containers/storage` `store/`
(which this handoff copies onto the disk). This is a deliberate trade-off required by the epic:
it costs a larger storage device in exchange for a build-time-built, self-contained recovery
source.

## Registry fallback — deferred alternative only

If the read-only additional-image-store model is ever found unworkable (the §6.6 spike fails),
the **deferred alternative** is a host-level non-CRI-O registry serving the on-disk copy plus a
digest `ImageDigestMirrorSet` (`localhost:5000`, `mirrorSourcePolicy: NeverContactSource`).
Only there do a registry image, a systemd unit, and an OCI-layout→registry-storage conversion
apply. The primary durability model above references no registry and no IDMS. See
[create-usb.md](create-usb.md#registry-fallback--documented-alternative-only).
