# USB-Based Image-Based Installation (IBI) for Disconnected SNO

> **Status:** Proposal / design draft
> **Scope:** SNO only. OCP **installed** from USB media with **zero network at install time** — no
> registry, web server, or network reachable while the node is being provisioned. Two media profiles:
> an **install-only warehouse pre-install** that lays OCP down on disk offline and is **personalized
> later at a mirror-connected site through the existing IBI data-image (virtual-media) mechanism**
> (primary), and a **self-contained, fully disconnected field deployment** that installs *and*
> personalizes from the USB and runs offline forever (secondary).

## Executive summary

Enable a Single Node OpenShift cluster to be **installed with zero network connectivity** from a
bootable USB stick. No registry, web server, or network is reachable *while the node is being
provisioned*. The two profiles differ in **how much of IBI the USB owns** — because they target
different sites:

- **Warehouse pre-install (primary) — install-only USB.** The USB does only the **installation** half
  of IBI (write seed/OCP to disk + precache images, offline), then powers off to ship.
  **Personalization happens later at a mirror-connected site through the existing IBI data-image
  mechanism** — the config ISO via virtual media, consumed by the unmodified reconfigure path. The
  site's external mirror is the runtime image source; the SNO runs no registry, and the USB carries **no
  site config and synthesizes no mirror artifact**.
- **Fully disconnected field deployment (secondary) — self-contained USB.** With no site
  infrastructure ever (no BMC, virtual media, or mirror), the USB installs *and* personalizes offline
  and the node **stays offline for life** — carrying the full four-partition payload (on-USB site
  config included) and its own read-only runtime image recovery source.

Scope is SNO only; the USB *install* needs no BMC/virtual media in either profile.

**Most of the flow already exists.** Installation is already two phases — *prepare* (bootable install
media) and *reconfigure* (first boot of the installed system). Primary uses **only** prepare from USB
and hands reconfigure to the existing site mechanism; secondary drives both from USB. Two needed
behaviors already work with little/no change: **site config delivery** (reconfigure already reads config
from an attached labeled device) and **power-off after prepare**.

**The genuinely new work is sourcing images offline during install** — today all image paths are a
network pull. The central new capability is importing images from the USB into the system's image store
**under their canonical names by digest** ([§5.1](#51-image-import-during-prepare)) so the installed
system resolves them locally. **Runtime durability then depends on the profile**
([§5.2](#52-local-resolution-and-runtime-durability)): images can be GC-evicted or wiped on reboot, and
something must restore them — the **external site mirror** for primary (a digest IDMS in the site's data
image), or a **read-only additional image store shipped on disk** for the forever-offline secondary (no
registry, no mirror).

**What the USB carries depends on the profile:** primary carries boot + images + a writable results
area (three); secondary adds on-USB site config (four). The secondary USB stays inserted through first
boot; both require a technician to force a one-time boot from the USB (the next reboot must land on the
installed disk).

**Automated USB creation is an in-scope deliverable** ([§6](#6-usb-creation-tooling-automated)): a
tool on a connected workstation that turns a single manifest into ready-to-boot media — resolve the
image set, mirror images, build the boot environment with install config embedded, render site config
(secondary), assemble the media, and emit an auditable digest manifest.

**Success detection without a console or network** ([§7](#7-successfailure-detection)): the
authoritative signal is each phase's exit status, delivered two ways — a result file + logs on the USB's
writable area (read off-box), and a power-state convention (success powers off / stays healthy; failure
halts powered-on). Install is binary (no rollback); remediation is re-run or re-image. The primary USB
reports only prepare; personalization success is reported by the site mechanism.

**Bottom line:** install flow, config delivery, and shutdown are largely there. The new work is offline
image sourcing and the automated USB tool (both profiles), plus — **secondary only** — the on-USB
reconfigure/pivot reboot and the read-only additional image store. Fitting a bootable install image and
large data partitions on one stick under Secure Boot is the main unknown ([§10](#10-open-questions--risks)).

## 1. Goal

Enable IBI to **install with zero network connectivity** using bootable USB media. The install is
offline in both profiles; they differ in how much of IBI the USB owns and in what the node has at
run time:

1. **Warehouse pre-install (primary) — install-only:** power on → boot USB → **prepare phase** (write
   OCP/seed to disk + precache images) → shut down → ship to site. There the node is **personalized
   later using the existing IBI data-image mechanism** (config ISO via virtual media, consumed by the
   unmodified reconfigure phase). The site has a **mirror registry external to the SNO**; at runtime the
   node is a standard mirror-connected SNO. The USB carries no site config.
2. **Fully disconnected field deployment (secondary) — self-contained:** power on → boot USB → **both
   phases run from USB** (prepare → pivot → reconfigure) → node is operational in a **fully disconnected
   environment and stays offline for life** — no upstream or mirror ever, so the USB carries the site
   config and the node carries its own runtime image recovery source.

> **Out of scope for scenario 2 — day-2 upgrades.** This design covers only the *install* of the
> self-contained field node. **Upgrading** that node afterward (IBU/image-based upgrade, or any other
> in-place update) is **out of scope** — the general "USB-based IBU/upgrade" non-goal below applies to
> the secondary profile in particular. A node that "stays offline for life" has no upstream or mirror
> to pull a new release from, so its lifecycle here ends at a working install; a disconnected upgrade
> path (e.g. a second USB carrying the next release closure) is a separate, future effort and is not
> assumed, designed, or validated here.

Non-goals (explicitly out of scope): hardware-agnostic seed images, USB-based IBU/upgrade,
multi-node (MNO) install, and USB-at-rest encryption.

> **BMC/virtual media and the two profiles.** The USB **install** step must work with a physically
> inserted stick and **no BMC/RedFish virtual media** in either profile — the offline-install
> requirement. This does **not** forbid virtual media at a connected site: the primary profile's
> *personalization* reuses the existing IBI data-image path, where the site delivers the config ISO by
> virtual media as for standard IBI. The constraint is on the USB install, not the site's later
> personalization.

> **Threat model — physical possession of the USB is trusted.** With at-rest encryption out of scope,
> site config **on the USB is unencrypted**. This applies only to the **secondary** profile, whose USB
> carries p3 (`cluster-config`) with the **cluster runtime pull secret (`siteConfig.pullSecret`)** in
> the clear. The design assumes that USB is handled as a trusted, controlled item through
> warehouse → ship → field (equivalent to handing over a pre-credentialed machine); if that does not
> hold, the pull secret must be provisioned via a protected activation step instead of baked into p3.
> The **primary** profile carries no site config on the USB, so this does not apply (its pull secret
> arrives in the site data image). Integrity/authenticity of the executing code rests on UEFI Secure
> Boot; content-partition signing is deferred ([§7.5](#75-media-integrity-secure-boot-signing-deferred)).

> **Scope note:** the original feature request listed automated USB creation as out of scope. It is now
> **in scope** — **automated USB creation tooling is a deliverable** ([§6](#6-usb-creation-tooling-automated)):
> a repeatable, parameterized tool, not a runbook.

## 2. How today's IBI maps onto the two USB flows

LCA's IBI is already a **two-phase, two-binary** design, which lines up well with the USB
flows:

| Phase | Command | Runs in | Responsibility |
|-------|---------|---------|----------------|
| **Prepare** | `lca-cli ibi -f <cfg>` | RHCOS live ISO | wipe disk → `coreos-installer install` → pull seed image → lay down ostree stateroot → precache container images (scope per profile, [§5.1](#51-image-import-during-prepare)) → optional `shutdown now` |
| **Reconfigure** | `lca-cli post-pivot` | first boot of installed disk | discover + mount site config → apply network/identity → recert → apply manifests → start cluster |

- **Warehouse flow (primary)** = the **prepare phase only**, with shutdown enabled. The reconfigure
  phase happens **later at the cell site through the existing IBI data-image mechanism** (config ISO
  via virtual media), not from the USB — so the USB owns only the top row of the table above.
- **Field flow (secondary)** = prepare phase → reboot into the installed disk → reconfigure phase,
  **all sourced from USB, all offline** — the USB owns both rows.

> **The field-flow reboot owner is net-new and must be explicit.** `IBIPrepare.Run()` ends at
> `shutdownNode()`, which runs `shutdown now` only when `IBIPrepareConfig.Shutdown` is true and
> otherwise **returns without rebooting** — there is no reboot in the prepare path today. In standard
> IBI the external image-based-install-operator (IBIO) owns the post-prepare reboot; the USB flow
> deliberately bypasses IBIO (no BMC/VirtualMedia), so nothing transitions the field flow from the live
> ISO to the installed disk. This feature must define that owner: the live-ISO ignition unit running
> `lca-cli ibi` **issues the pivot reboot after prepare succeeds when `Shutdown` is false** (warehouse
> `Shutdown: true` powers off instead — mutually exclusive terminal actions). Tracked as
> [§8.I](#8-work-items), with a field-flow test covering the live-ISO→installed-disk transition.

Key references:

- Prepare entry: `lca-cli/cmd/ibi.go` (`runIBI`, lines 55-82), config loader
  `utils/ibi_config.go` (`ReadIBIConfigFile`, lines 10-30).
- Prepare flow: `lca-cli/ibi-preparation/ibipreparation.go` (`Run`, lines 53-86;
  `diskPreparation`/`coreos-installer install`, lines 182-207; seed pull, lines 59-62;
  `precacheFlow`, lines 88-137; `shutdownNode`, lines 139-149).
- Reconfigure flow: `lca-cli/postpivot/postpivot.go` (`PostPivotConfiguration`, lines 124-255).
- Config structs: `api/ibiconfig/ibiconfig.go` (`ImageBasedInstallConfig` 24-65,
  `IBIPrepareConfig` 67-135); `api/seedreconfig/seedreconfig.go` (`SeedReconfiguration`, 25-173).

## 3. What we get for free

Two existing mechanisms already satisfy USB requirements with **little or no code change**:

### 3.1 Config delivery by labeled block device (already works)

The reconfigure phase already discovers site config by scanning block devices for the FS
label `cluster-config` (`postpivot.go:912-944`, label from `seedreconfig.go:8`), mounting it,
copying the `/opt/openshift/...` tree out, then unmounting (`setupConfigurationFolder`,
`postpivot.go:947-964`). **A USB partition labeled `cluster-config` containing the standard
layout is consumed today with no code change.**

The **primary (install-only) profile** relies on this same path at the site: the `cluster-config`
device it finds there is the site's **data image** (config ISO) via virtual media, not a USB partition.
The reconfigure code does not care which — it discovers the label, mounts, copies — so primary
personalization is the **unmodified existing mechanism**. Only the **secondary** profile supplies
`cluster-config` as an on-USB partition (p3).

Expected layout on the config partition (constants in `internal/common/consts.go`):

```text
/opt/openshift/
  cluster-configuration/
    manifest.json          # SeedReconfiguration (cluster identity, network hints, pull secret, ...)
    manifests/             # cluster manifests
    kubeconfig-crypto/     # recert crypto material
  network-configuration/
    system-connections/    # .nmconnection files
  extra-manifests/         # optional extra manifests
```

`SeedReconfiguration` (`seedreconfig.go:25-173`) already carries every site-specific field the
disconnected flow needs: `Hostname`, `NodeIPs`, `MachineNetworks`, `RawNMStateConfig` (nmstate
YAML applied at first boot — the IBI path), `PullSecret`, `SSHKey`/`ServerSSHKeys`,
`ClusterName`, `BaseDomain`, `Proxy`, `AdditionalTrustBundle`, `ChronyConfig`, `NodeLabels`.
Note: there is **no dedicated DNS field** — DNS rides inside `RawNMStateConfig` / nmconnection
files; cluster-internal DNS is served locally by dnsmasq (`postpivot.go:710-729`), so it works
offline.

### 3.2 Shutdown-on-completion for the warehouse flow (already works)

The warehouse "prepare → shut down → ship" behavior is exactly the existing
`IBIPrepareConfig.Shutdown` field → `shutdownNode()` (`ibiconfig.go` field ~95-98,
`ibipreparation.go:139-149`), which runs `shutdown now` as the final prepare step. No new work
is needed for warehouse shutdown; it needs only documentation and a validated config.

> Note: shutdown-on-completion exists **only** for the prepare phase — there is no
> shutdown-after-reconfigure path (`PostPivotConfiguration` ends with `cleanup()` and boot continues),
> and the reboot client only reboots, never powers off. The field flow wants the node *operational*
> after reconfigure, so this is fine; a "reconfigure then power off" mode would be new work.

## 4. USB media layout

The creation tool ([§6](#6-usb-creation-tooling-automated)) assembles **one image written to the
stick** (p1 bootable, the rest data). The partition set **depends on the profile**:

- **Primary (install-only):** **p1 + p2 + p4** — boot environment, images, writable results. **No
  p3**: site config is delivered later at the site as the IBI data image (virtual media), not on the
  USB.
- **Secondary (self-contained):** **p1 + p2 + p3 + p4** — adds the on-USB `cluster-config` partition,
  since a fully disconnected field site has no data-image/virtual-media path.

The USB is *not* the install target — the prepare phase writes the OS to a separate `InstallationDisk`
(e.g. `/dev/disk/by-id/...`); the USB only carries the boot environment, the images, (secondary) the
config, and a writable area for results.

> **This four-partition layout is provisional until proven bootable.** Whether a bootable isohybrid
> live ISO can coexist with the p2/p3/p4 data partitions on one stick — under UEFI Secure Boot and BIOS
> fallback on reference firmware — is unresolved (the format-collision problem in
> [§10](#10-open-questions--risks)). A **bootability spike covering p1–p4 on reference Dell / HPE GNR-D
> hardware is a gate** before the media contract and the assembly step
> ([§6.4](#64-steps-the-tool-performs) step 5) are finalized. If no single-stick assembly proves
> bootable, the layout changes (images inside the ISO, or a two-stick fallback) — treat the table below
> as intended design, not a settled contract.

| Part | FS / type | Label | Contents | Consumed by |
|------|-----------|-------|----------|-------------|
| p1 | ISO9660 / EFI (El Torito) | — | RHCOS live ISO + embedded ignition that auto-runs `lca-cli ibi -f <cfg>` | firmware boot → live environment |
| p2 | xfs/ext4 (may be read-only) | `ibi-images` | OCI image layout: seed image + **all** referenced container images (release payload, operators, recert, lca-cli) | **prepare** — mounted, then imported into `/var/lib/containers` under canonical names ([§5.1](#51-image-import-during-prepare)) |
| p3 (**secondary only**) | ext4/vfat | `cluster-config` | `/opt/openshift/...` reconfigure tree (`SeedReconfiguration`, `manifests/`, `kubeconfig-crypto/`, net config, `extra-manifests/`) | **reconfigure** — found by FS label (primary: same device arrives as the site data image) |
| p4 | ext4 (**writable, journaled**) | `ibi-status` | per-phase records (`prepare.json`; `reconfigure.json` self-contained only) + logs | **prepare**; **reconfigure** (self-contained) — found by FS label; atomic writes ([§7.4](#74-the-ibi-status-partition-contract-discovery--persistence)) |

The **prepare config** itself (`IBIPrepareConfig`: `SeedImage`, `InstallationDisk`, `Shutdown`, plus
the new `LocalImagesPath`/`Disconnected`/`ExpectedDigestsPath` fields from [§8.A](#8-work-items)) is
embedded in the p1 live-ISO **ignition** rather than on its own partition — keeping the flow zero-touch
(config travels with the boot media). Identical for both profiles; the only difference is whether p3 is
present on the USB.

Why these, specifically:

- **p1** is standard `coreos-installer` output; LCA does not build the live ISO today
  (`RHCOSLiveISO` is normally a `mirror.openshift.com` URL), so USB creation tooling ([§6](#6-usb-creation-tooling-automated))
  must produce it and embed the auto-run ignition.
- **p2** is the whole point of [§5](#5-the-core-problem-zero-network-image-sourcing) — the offline
  image source. Its discovery/mount contract mirrors p3/p4's labeled-block-device approach: the
  live-ISO prepare unit locates p2 by the FS label `ibi-images`, mounts it **read-only**, and sets
  `LocalImagesPath` before `lca-cli ibi` runs. Discovery is a **hard precondition** (unlike best-effort
  p4): if no `ibi-images` device is found, or **more than one** matches, the unit **fails fast before
  any disk write** rather than pull from a network that does not exist. Released after precache
  ([§8.C](#8-work-items)).
- **p3 (secondary only)**'s label is **not arbitrary**: `post-pivot`'s `waitForConfiguration`
  (`postpivot.go:912-944`) scans block devices for exactly the `cluster-config` label
  (`seedreconfig.go:8`), so the existing mount-and-copy code consumes the partition with **zero change**
  ([§3.1](#31-config-delivery-by-labeled-block-device-already-works)). The primary profile ships no p3 —
  the same `cluster-config` device arrives at the site as the data image, via the identical code path.
- **p4** is the only *writable* area on the media: with no console or network, each phase writes its
  result marker + logs here for a technician to read off-box ([§7](#7-successfailure-detection)). The
  tool creates it empty ([§6](#6-usb-creation-tooling-automated)) so the first write has a target.

### 4.1 First-boot / config discovery per profile

`waitForConfiguration` runs on the **first boot of the installed disk**, not in the live ISO. Where
it finds `cluster-config` differs by profile:

- **Secondary (self-contained):** the USB **remains physically inserted through the pivot reboot**
  into the installed disk, so `post-pivot` discovers `cluster-config` (p3) on the still-present USB.
  This matches the zero-touch model ("plug in, power on, walk away") and keeps p3 as the single source
  of site config for the fully offline flow.
- **Primary (install-only):** the warehouse box powers off after prepare and ships **without needing
  the USB thereafter**. At the site, `cluster-config` is provided as the **data image via virtual
  media** through the existing mechanism; the USB need not remain inserted for personalization.

> **Note — the technician must force the initial boot from the USB.** The prepare phase only starts if
> firmware boots the USB's live ISO (p1). The technician selects the USB as a **one-time boot device**
> (UEFI/BIOS boot menu) or first in boot order; do not rely on default order, since the
> `InstallationDisk` may already carry a bootable OS. This is a **one-time** action: the pivot reboot
> after prepare must land on the *installed disk*, **not** re-boot the USB.
>
> **Boot selection is guidance, not the safety control — prepare guards itself against re-entry.**
> Firmware boot order is outside LCA's control and one-time selection can be defeated. A second USB boot
> would otherwise re-run `lca-cli ibi` and **wipe the freshly installed disk**. So before any destructive
> action (`cleanupDisk` / `coreos-installer install`), prepare reads a **durable prepare-complete
> marker** — the `prepare.json: success` record on p4 from the first run — and, if present, **refuses to
> wipe**: it records the re-entry as `pivot: failed` ([§7.2](#72-result-write-back-primary-signal)) and
> **halts powered-on** ([§8.C](#8-work-items)). The marker rides on the same stick, so it survives every
> subsequent USB boot. This on-media guard, not boot order, is what prevents the data-loss path.

Implications: no code is needed to copy config onto the installed disk before reboot — the existing
labeled-block-device discovery works as-is (USB p3 for secondary, site data image for primary). For
secondary, the removable USB must remain enumerable as a block device after the pivot reboot (true for
a physically inserted stick).

## 5. The core problem: zero-network image sourcing

This is the one hard requirement with **no existing support**. Today every image path in LCA is a
network `podman pull`: the seed image (`ibipreparation.go:60`) and precache (`podmanImgPull`,
`pullImages.go:57`). There is **no `oci:` / `dir:` / `containers-storage:` / local-registry
transport** in first-party code; the only "local" concepts are `ostree pull-local` (OS filesystem,
not images) and `podman image mount` of an already-pulled seed.

What *does* exist is a **registry-hostname remap** path — `IBIPrepareConfig.ReleaseRegistry` +
`ShouldOverrideSeedRegistry` + `ReplaceImageRegistry` (`utils/client_helper.go:275-425`,
`utils/utils.go:263`) — plus `registries.conf` parsing (`ibipreparation.go:238-258`) and the
`ImageDigestSources { Source, Mirrors }` shape (`ibiconfig.go:137-143`). It is **not** reusable here:
`ReplaceImageRegistry` *rewrites* the reference hostname, storing images under non-canonical names —
the defect that rules out local import ([§5.1](#51-image-import-during-prepare)). The design instead
adds a `containers-storage:` import path.

### 5.1 Image import during prepare

Prepare must leave **all required images resident in the persistent `/var/lib/containers` partition,
stored under their canonical names by digest** — the property
[§5.2](#52-local-resolution-and-runtime-durability) depends on for both flows. Storage *naming* is the
make-or-break detail: an image under its canonical name (`quay.io/…@sha256:…`) is the one a pod resolves
locally with no mirror, and the one a runtime IDMS recovers by redirecting canonical → mirror. How
images get there during prepare is separate from the runtime durability artifact (§5.2).

> **What the precache set covers differs by profile.** Today `precacheFlow` imports only the seed's
> `containers.list`; prepare instead drives from the on-media precache list `create-usb` emits
> ([§6.4](#64-steps-the-tool-performs) step 1, [§8.C](#8-work-items)):
>
> - **Primary (install-only):** the **seed closure** (`containers.list` + recert + lca-cli) **plus any
>   optional extras**. Site-specific images ride in the site data image and pull from the external
>   mirror, so they are **not required offline** — the primary precache is a *warm-start optimization*,
>   and its self-check asserts the seed closure + extras.
> - **Secondary (self-contained):** the **full closure** — seed list + recert + lca-cli **plus every
>   image referenced by the shipped p3 manifests/extra-manifests** — because a forever-offline node can
>   never pull. Every image must be imported during prepare; the self-check fails if any is missing.

#### Chosen — direct `containers-storage` import

1. Mount the USB OCI layout (p2, [§4](#4-usb-media-layout)).
2. For the seed and every image in the closure, import it directly into the target container store
   **under its canonical name by digest**:
   `skopeo copy oci:<usb-layout>:<ref> containers-storage:[overlay@/var/lib/containers+...]<canonical>@sha256:D`.
3. **Two destination stores, sequenced by when they exist:**
   - **Seed → live-ISO store first.** `SetupStateroot` (`ibipreparation.go:66-71`) consumes the seed
     *before* any installed stateroot exists — the seed pull today (`ibipreparation.go:60`) precedes
     stateroot setup for exactly this reason. So the seed replaces that `podman pull` with a
     `containers-storage` import into the **live** graphroot (no chroot, no `/mnt` prefix): a bootstrap
     input to stateroot deployment, not a runtime artifact, dropped from the live store afterward.
   - **Closure (release + user workloads) → *installed* stateroot's `/var/lib/containers`**, not the
     live-ISO store — else those images vanish at the pivot. This reuses the existing precache
     mechanism: prepare sets `OstreeDeployPathPrefix=/mnt/`, deploys the stateroot on the mounted
     install disk, and `precacheFlow` **chroots to `/host`** before `workload.Precache`
     (`ibipreparation.go:122-132`), so writes land in the persistent on-disk store. The
     `containers-storage` import targets that **same** store, so images imported during prepare are the
     images CRI-O finds after first boot.

Because the destination reference is canonical, storage is canonical **by construction** — no
registry, no `registries.conf`, no reference rewriting. This satisfies
[§5.2](#52-local-resolution-and-runtime-durability) identically for both profiles: the normal path is a
local hit under the canonical name. Recovery differs by profile: **primary** re-pulls from the site
mirror via the site's runtime IDMS (from the data image); **secondary** finds the image still in its
read-only additional image store under the *same* canonical name — **no re-pull, no IDMS**. The
prepare-time cost is a new local-import path at the two pull sites — `podmanImgPull`
(`pullImages.go:57`) and the seed pull (`ibipreparation.go:60`) — targeting the mounted store by digest
instead of `podman pull` ([§8.B](#8-work-items)/[§8.C](#8-work-items)).

> **Rejected — local registry + `ReleaseRegistry` remap.** The tempting shortcut — run a
> `localhost:5000` registry over the layout and reuse `ReleaseRegistry` so precache "just works" —
> fails. `ReleaseRegistry` drives `ReplaceImageRegistry` (`utils/utils.go:263`), a regex that
> **rewrites the reference hostname**, so images land under `localhost:5000/…`, *not* canonical names.
> That breaks the no-mirror normal path ([§5.2](#52-local-resolution-and-runtime-durability)) and
> strands the **primary** flow: a runtime IDMS mapping canonical → the external site mirror matches
> nothing in a `localhost:5000`-named store, so every image re-pulls at first boot. Its one apparent
> benefit (reusing `ReleaseRegistry`) is the defect.

### 5.2 Local resolution and runtime durability

However prepare sources the images ([§5.1](#51-image-import-during-prepare)), the **installed node**
must resolve every image locally at run time, and must **keep** it resolvable against image garbage
collection and on-disk corruption. Two facts hold for both profiles:

- **Local resolution needs canonical names.** An image present in `/var/lib/containers` under the
  **same canonical name and digest** a pod references is used directly by CRI-O — no registry
  contact, no mirror lookup. This is the normal-operation path, and it requires images to be stored
  under canonical names by digest (the direct import [§5.1](#51-image-import-during-prepare) does this
  by construction). On this path neither flow contacts any registry.
- **A single cached copy is not durable.** Images in containers-storage are subject to kubelet
  **image garbage collection** (unused images evicted under disk pressure, above
  `imageGCHighThresholdPercent`) and to on-disk corruption. Worse, **CRI-O removes corrupted images on
  reboot** — its storage integrity check wipes invalid images so they re-pull on next use — so a corrupt
  image is not merely unreadable, it is *deleted* on the next boot. On a connected node this is harmless
  (it re-pulls). So the design needs a **recovery source** for evicted or corruption-wiped images — and
  *where that source lives is what differs between the two flows.*

**Primary profile — recovery is the site's job, not the USB's.** The warehouse-prepared node activates
at a site with a **mirror registry external to the SNO** — a standard mirror-connected SNO at run time.
The digest **`ImageDigestMirrorSet`** mapping each canonical source registry → the site mirror arrives
in the **site's standard disconnected config, delivered as the data image**
([§3.1](#31-config-delivery-by-labeled-block-device-already-works)) — **`create-usb` ships none of it**.
Normal resolution stays local (images precached under canonical names); the site IDMS's
job is **recovery**: a GC-evicted or corruption-wiped image re-fetches from the site mirror via CRI-O's
standard pull path — automatic across a reboot, no upstream internet, exactly how disconnected OpenShift
already works. **The SNO runs no registry**, and the USB's only obligation is **canonical storage**
([§5.1](#51-image-import-during-prepare)) so the site IDMS resolves.

**Secondary profile — recovery must live on the node, shipped by the USB.** A forever-offline node has
**no external mirror ever** and no data image, so the recovery source is carried on the node: a
**persistent read-only additional image store**. `create-usb` builds a second canonical copy of the
closure as a **read-only `containers/storage` store**, shipped on the media and installed on the disk
(outside CRI-O's writable graphroot; handoff in [§8.D](#8-work-items)) and registered via
`additionalimagestores` in `storage.conf`. CRI-O then resolves every closure image from it by canonical
name/digest with **no registry and no mirror on any path** — initial resolution and recovery alike.
This survives both failure modes across a reboot: kubelet GC and CRI-O's reboot-time corruption cleanup
act **only on the writable graphroot**, leaving the read-only copy untouched. Cost: roughly **2×** the
image footprint and **no long-running service**. This is the single durability model for secondary; no
registry, no IDMS.

**Why not an on-node registry (bootstrap circular dependency).** A rejected alternative is to run a
persistent `localhost:5000` registry over the on-disk copy plus a digest IDMS redirecting canonical →
`localhost:5000`. That model has a **circular dependency**: the registry itself runs from a container
image in CRI-O's store, so the very GC eviction / reboot-time corruption cleanup the registry exists
to recover from can delete the *registry's own* image — and the IDMS cannot restore it, because the
only recovery endpoint is the registry that will not start. The read-only additional image store has
**no process to bootstrap**: the store is consulted directly at container-create time, lives outside
the GC/corruption path, and needs neither a registry image in the closure nor an IDMS.

**Registry fallback (pending a compatibility spike).** The additional-store copy must be in
`containers/storage` **overlay** format matching the node's CRI-O, which `create-usb` produces with
`skopeo copy oci:<layout> containers-storage:[overlay@<store>]<ref>@sha256:D`; this needs a
compatibility spike against the target RHCOS/CRI-O ([§6.6](#66-open-tooling-questions)). If it does
not hold, the fallback is an on-node registry **run outside CRI-O** (host-level systemd service — a
static registry binary, or podman with a **separate `--root`** — serving the on-disk copy, plus a
digest IDMS → `localhost:5000` with **`mirrorSourcePolicy: NeverContactSource`**). Running it as a
*host* process keeps its binary and storage outside the GC/corruption path, breaking the circular
dependency. Documented fallback, not the primary model.

Build-time digest guarantee — scope differs by profile:

- **Primary:** `create-usb` resolves the seed closure + declared extras to digests for p2; there are
  **no p3 manifests on the USB to rewrite**. Digest-pinning the *site* manifests (and site IDMS) is the
  site's responsibility, delivered in the data image — standard disconnected-OpenShift discipline.
- **Secondary:** `create-usb` resolves every reference to a digest **and rewrites the shipped p3
  manifests to digest form** ([§6.4](#64-steps-the-tool-performs) steps 1 and 4), failing the build on
  any reference that cannot be pinned — so no tag path exists at runtime.

> **Out of scope — whole-disk failure and durable-copy bit-rot.** No model survives loss of the single
> SNO disk (inherent to single-disk hardware; a re-provision scenario), nor corruption of the *sole
> durable copy itself* (bit-rot of the read-only store, or the fallback's registry storage) with no
> network. The models address the *recoverable* modes — GC eviction and corruption of the **writable
> working copy**, which the read-only copy restores — not loss or corruption of the durable copy.

For **primary**, runtime durability is standard mirror-connected SNO behavior owned by the site, so the
USB's obligation is just canonical storage. For **secondary**, the read-only additional image store +
`storage.conf` drop-in are net-new (LCA ships no `storage.conf` durability config today) — the
highest-risk item. Both are proven by the profile-specific recovery e2e in [§9](#9-phasing).

> **Limitation — runtime-generated tag references are unsupported (secondary profile).** Build-time
> digest-pinning covers everything on the media (the seed payload and the shipped p3
> manifests/extra-manifests, rewritten to digests in [§6.4](#64-steps-the-tool-performs) step 4) but
> **cannot** cover an image reference *minted at runtime* with a **tag** — e.g. an operator that
> creates a `Deployment` with a `name:tag` image, or a workload applied after install. Only a **digest**
> `ImageDigestMirrorSet` is shipped (no `ImageTagMirrorSet`), so a runtime tag cannot resolve even
> though the image is present by digest. Supported workloads must reference images by digest;
> digest-pinned operator catalogs and the release payload satisfy this, arbitrary tag-minting operators
> do not — call this out in user docs. *(The primary profile is a normal mirror-connected SNO at
> runtime, where a stray tag can resolve through the site mirror if it ships an `ImageTagMirrorSet` —
> so this is only the usual disconnected-OpenShift guidance there, not a hard offline constraint.)*

## 6. USB creation tooling (automated)

Automated, repeatable creation of the USB media is an in-scope deliverable. The tool runs on a
**connected provisioning workstation** (with registry access and pull secret) — never on the
target node — and turns a single input manifest into ready-to-boot USB media.

### 6.1 Prerequisites

The provisioning workstation running `lca-cli create-usb` must have all of the following reachable
(the target node never needs them — only the workstation, at media-build time):

1. **Seed image created and available** — the SNO seed image (`seedImage`) has been generated (see
   [seed-image-generation.md](../seed-image-generation.md)) and pushed to a registry the workstation
   can pull from.
2. **HTTPS server hosting the RHCOS live ISO** — the tool fetches the base live ISO (p1) over **HTTPS**
   and verifies its published checksum/signature before use (plain HTTP or unverified downloads
   rejected — [§7.5](#75-media-integrity-secure-boot-signing-deferred)).
3. **Container image registry reachable** — the registry/mirror holding the seed's referenced images
   (release payload, operators, recert, lca-cli), so the tool can mirror them into the p2 OCI layout
   (`--auth-file`).
4. **A valid pull secret** — credentials for the seed and image registries (via `--auth-file`).

### 6.2 Form

Recommended: a new **`lca-cli create-usb`** subcommand alongside the existing `create`
(`lca-cli/cmd/`). It reuses in-tree building blocks — the seed `containers.list` logic
(`seedcreator.go:282`), config/ops helpers, and the `containers/image` + `coreos-installer` toolchain
— and ships in the same binary operators already run. Alternatives: a `hack/create-usb.sh` that
graduates to Go, or a container image for CI/pipeline use. Whatever the form, the logic below is the
contract.

### 6.3 Inputs (single manifest)

A YAML manifest describing one USB, e.g.:

```yaml
mode: install-only                    # install-only (primary) | self-contained (secondary)
seedImage: <registry>/<seed>@sha256:...
seedVersion: 4.1x.y
releaseVersion: 4.1x.y
installationDisk: /dev/disk/by-id/... # target node's disk (for embedded ignition)
disconnected: true                    # zero-network image sourcing (both profiles)
shutdown: true                        # power off after prepare (always true for install-only)
rhcosLiveIso:                         # p1 source (maps to ImageBasedInstallConfig.RHCOSLiveISO)
  url: https://mirror.openshift.com/.../rhcos-live.x86_64.iso
  sha256: 3b1e...                     # required; build fails if the fetched ISO doesn't match

# --- install-only (primary) — optional; site config is NOT on the USB ---
extraPrecacheImages:                  # optional warm-start images beyond the seed closure;
  - quay.io/.../foo@sha256:...        #   site-only images otherwise pull from the site mirror

# --- self-contained (secondary) — the following are REQUIRED in this mode ---
# runtime durability is fixed: create-usb always ships a read-only additional image store (no registry/IDMS)
clusterManifests:                     # -> p3 cluster-configuration/manifests/ (applied by post-pivot)
  - path: ./manifests/               # dir or file; refs digest-rewritten & closure-scanned (steps 1,4)
extraManifests:                       # -> p3 extra-manifests/
  - path: ./extra-manifests/
siteConfig:                           # -> p3 SeedReconfiguration + network
  clusterName: ...
  baseDomain: ...
  hostname: ...
  nodeIPs: [ ... ]
  machineNetworks: [ ... ]
  rawNMStateConfig: |
    ...
  sshKey: ...
  pullSecret: ...                     # the running CLUSTER's pull secret, baked into p3
```

**`mode` (profile selector).** `install-only` builds **p1 + p2 + p4** and requires only the
prepare/image inputs above; `self-contained` builds **p1 + p2 + p3 + p4** and additionally requires
`clusterManifests`/`extraManifests` and `siteConfig`. `create-usb` **fails the build** if a
`self-contained`-only field appears under `install-only` (or vice-versa). In `install-only` the
manifest is just `IBIPrepareConfig` (+ the new `LocalImagesPath`/`Disconnected` fields,
[§8.A](#8-work-items)) plus optional `extraPrecacheImages`; `self-contained` also unions in
`SeedReconfiguration`.

**Manifest validation rules (`create-usb` fails the build on any violation).** Beyond the
field-presence check above, the tool enforces the invariants the two flows depend on rather than
leaving them to chance ([§8.A](#8-work-items) mirrors these in `IBIPrepareConfig.Validate()`):

- **`disconnected` must be `true`.** The feature *is* zero-network image sourcing; a missing or
  `false` value would leave a network-pull path live in `IBIPrepareConfig` on the node. `create-usb`
  requires `disconnected: true` for both profiles and rejects anything else — it is not a knob.
- **`shutdown` must match `mode`.** `install-only` requires `shutdown: true` (power off, ship to site);
  `self-contained` requires `shutdown: false` (on-USB pivot reboot into reconfigure). Both mismatches
  are rejected — otherwise the primary flow could pivot with no site config, or the secondary flow
  could power off before reconfigure ([§6.4](#64-steps-the-tool-performs) step 3, [§8.I](#8-work-items)).
- **`self-contained` requires a non-empty `siteConfig.pullSecret`.** MCO needs a runtime pull secret
  even fully offline, and the forever-offline node has no other source. The build fails if it is
  absent, empty, or (for an `@file` reference) missing/unparseable. For `install-only` it is **absent
  by design** — the runtime pull secret arrives later in the site data image.
- **`self-contained` rejects mirror resources that can reach the network (CWE-16).** `clusterManifests`
  and `extraManifests` are copied to p3 and **applied verbatim by `post-pivot`**, so a user-supplied
  `ImageDigestMirrorSet`/`ImageContentSourcePolicy` becomes live CRI-O config on the forever-offline
  node. If one lists an **external** mirror, or leaves source fallback enabled (`mirrorSourcePolicy`
  unset or `AllowContactSource`), CRI-O tries that network endpoint on a local miss — silently breaking
  the zero-network guarantee. So `create-usb` **scans the rendered p3 manifests and fails the build** on
  any IDMS/ICSP unless **both**: every `mirrors` entry is the on-node local endpoint (the
  additional-store model ships **no** IDMS, so any IDMS is already suspect; the registry fallback allows
  only `localhost:5000`), **and** `mirrorSourcePolicy: NeverContactSource` is set. *(N/A to
  `install-only`, which ships no p3.)*

**Install-only carries no site config.** All personalization inputs (`siteConfig`, `clusterManifests`,
runtime pull secret, mirror/IDMS config) are delivered **later, at the site, in the data image**
([§3.1](#31-config-delivery-by-labeled-block-device-already-works),
[§5.2](#52-local-resolution-and-runtime-durability)) — `create-usb` renders no p3 and synthesizes no
mirror artifact. `extraPrecacheImages` is the *only* content knob (a warm-start optimization; anything
not precached pulls from the site mirror at first boot).

**Runtime durability artifact (secondary only, fixed).** For `self-contained`, `create-usb` always
builds ([§6.4](#64-steps-the-tool-performs) step 4) a **read-only `containers/storage` store** from
the closure + a `storage.conf` drop-in registering it under `additionalimagestores` — no selector, the
single durability model, **no registry and no IDMS**. If the compatibility spike fails
([§6.6](#66-open-tooling-questions)), the fallback instead synthesizes a digest IDMS (canonical →
`localhost:5000`) plus a host-level registry. (Primary has no such artifact — its site mirror/IDMS
comes from the site data image.)

**`rhcosLiveIso` (p1 source).** `create-usb` must select and validate p1 deterministically, so the
ISO source is an explicit field (mapping to the existing `ImageBasedInstallConfig.RHCOSLiveISO`),
not implicit. It carries the fetch `url` (HTTPS only) and a **required** `sha256` the tool verifies
before use ([§6.1](#61-prerequisites)); a mismatch or missing checksum fails the build. A
`--rhcos-live-iso`/`--rhcos-live-iso-sha256` **CLI flag overrides the manifest value** when both are
present (flag wins), so a pipeline can pin a locally-staged ISO without editing the manifest.

**p3 manifest inputs (`clusterManifests` / `extraManifests`) — secondary only.** These name the source
paths (or inline documents) `create-usb` copies into `cluster-configuration/manifests/` and
`extra-manifests/` — the same files steps 1 and 4 scan and rewrite to digests. Contract: each entry is
a file or directory; directories expand **non-recursively in lexical order** (deterministic output);
entries are copied verbatim (no merge) with a **destination-name collision failing the build** rather
than silently overwriting. *(Absent in `install-only` — the equivalent manifests ride in the site data
image.)*

**Pull secrets — which apply depends on the profile:**

- The **workstation mirror credentials** (pull seed + mirror images into p2) are a **CLI flag**
  (`--auth-file`), never a manifest field and never written to the USB. Applies in **both** profiles.
- The **cluster runtime pull secret** applies to **both profiles, delivered differently**:
  `self-contained` bakes `siteConfig.pullSecret` into p3; `install-only` receives it in the **site data
  image** at personalization. Either way the installed node needs a valid pull secret even fully
  offline (MCO requires one).

For `self-contained`, `siteConfig.pullSecret` accepts **literal JSON** or, when prefixed with `@`, a
**file reference** that `create-usb` resolves **on the workstation at build time** — reading the file
and baking the literal contents into p3's `manifest.json`. `create-usb` **must fail** if an
`@`-referenced file is missing or unparseable. The `@` prefix is a build-time convenience only; it
never appears in the rendered p3 manifest.

Likewise the **output destination** is a CLI flag (`--output` for a `.img`, or `--write-device`
for a block device), not a manifest field.

### 6.4 Steps the tool performs

1. **Resolve the image set (scope depends on `mode`).**
   - **`install-only` (primary):** the **seed closure** — the seed image's `containers.list` (the same
     list `seedcreator.go:282` produces at seed-creation time) + recert + lca-cli — **plus any
     `extraPrecacheImages`** named in the manifest. There are no p3 manifests to scan; site-specific
     images ride in the site data image and pull from the site mirror if not precached
     ([§5.1](#51-image-import-during-prepare)).
   - **`self-contained` (secondary):** the **full closure** — the seed closure **plus every image
     referenced by the shipped p3 manifests** (`cluster-configuration/manifests/` and
     `extra-manifests/`, per `api/imagebasedupgrade/v1/types.go`), including operator
     `CatalogSource`/bundle images, `Deployment`/`Pod`/`DaemonSet` images, and any images named in
     `ImageDigestMirrorSet`/`ImageContentSourcePolicy` entries. The primary durability model (a
     read-only additional image store, [§5.2](#52-local-resolution-and-runtime-durability)/[§8.D](#8-work-items))
     adds **no** image to the closure — it has no registry to run; only the **registry fallback** adds
     the registry service's own image. Anything missing from p2 would become a live pull the
     forever-offline node cannot satisfy, so `create-usb` scans the rendered p3 tree and adds its
     references.

   In both modes, `create-usb` **resolves every reference to a digest**, failing the build if any
   cannot be pinned ([§5.2](#52-local-resolution-and-runtime-durability)). The resolved digest set
   drives the p2 mirror (step 2), the p3 manifest digest-rewrite (step 4, secondary only), and the
   on-media digest manifest (step 6) that prepare uses as both its **precache list**
   ([§8.C](#8-work-items)) and its **self-check** reference ([§7.1](#71-per-phase-success-criteria)).
2. **Build the p2 OCI layout.** Mirror the seed + every image in that set into an OCI image
   layout (`oc-mirror --v2` or `skopeo copy docker://… oci:…`), using the workstation pull
   secret. Preserve digests.
3. **Build the p1 live ISO.** Fetch the RHCOS live ISO from `rhcosLiveIso` (verify its `sha256`
   before use — [§6.3](#63-inputs-single-manifest), [§6.1](#61-prerequisites)) and embed ignition
   that auto-runs `lca-cli ibi -f <embedded-cfg>` on boot, where `<embedded-cfg>` is the
   `IBIPrepareConfig` derived from the manifest (with `Disconnected: true`, `LocalImagesPath` pointing
   at the mounted p2). For `install-only` the unit ends in `shutdown` (`Shutdown: true`); for
   `self-contained` it issues the **pivot reboot** into the installed disk (`Shutdown: false`,
   [§8.I](#8-work-items)) so the on-USB reconfigure phase can run. `coreos-installer iso ignition
   embed` / `iso customize`.
   **Package the `lca-cli` executable into p1** — the stock live ISO ships no `lca-cli`, so v1 embeds
   the static binary via ignition (details and rejected alternative in [§8.E](#8-work-items)).
4. **Build the p3 config tree — `self-contained` only.** *(Skipped for `install-only`, which ships no
   p3; its personalization comes from the site data image.)* Render the `/opt/openshift/...` layout
   from `siteConfig`: `cluster-configuration/manifest.json` (`SeedReconfiguration`),
   `network-configuration/` nmconnection files, `extra-manifests/`. **Rewrite every image reference in
   the rendered manifests to its resolved digest** (`name:tag` → `name@sha256:...`) — mirroring by
   digest (step 1) is not enough, since the durable copy is stored by digest and nothing resolves a
   shipped **tag** → digest offline ([§5.2](#52-local-resolution-and-runtime-durability)). A reference
   that cannot be pinned is **unsupported**; `create-usb` fails the build rather than ship media that
   pulls at runtime.
   **Build the runtime durability artifact and stage it on the media.** The installed stateroot does
   not exist yet at build time, so `create-usb` cannot write into it — it builds a **read-only
   `containers/storage` store** from the closure (`skopeo copy oci:<layout>
   containers-storage:[overlay@<store>]...`), validates it against the target CRI-O, and ships it
   **read-only on p2** at `store/` (beside the OCI layout and `digests.json`) with a rendered
   `storage.conf` drop-in. *Prepare* then performs the handoff onto the installed disk — the full
   build → carry → install contract (paths, mount, ownership, path-rewrite) is in [§8.D](#8-work-items).
   Host-level filesystem artifact, not a cluster manifest: no IDMS, no `post-pivot` apply, no registry.

   If the compatibility spike fails ([§6.6](#66-open-tooling-questions)), the fallback instead emits a
   digest **`ImageDigestMirrorSet`** (canonical → `localhost:5000`, `mirrorSourcePolicy:
   NeverContactSource`, never an `ImageTagMirrorSet`) into `cluster-configuration/manifests/` plus a
   host-level registry service. Since `post-pivot` runs `deleteAllOldMirrorResources` before applying
   that dir, the IDMS must be in the applied set (re-created after the delete), not a pre-existing CR.
5. **Assemble the media (partition set per `mode`).** Create the output image (or write to the block
   device): write the live ISO to the front (p1), then create p2 labeled `ibi-images` populated with
   the OCI layout, and an **empty, writable** p4 labeled `ibi-status` (result marker + logs written at
   prepare/reconfigure time — [§7](#7-successfailure-detection)). For `self-contained` **also** create
   p3 labeled `cluster-config` populated with the tree from step 4; `install-only` omits p3.
6. **Report & embed the digest manifest.** Emit the resulting size (validates the USB-size budget,
   [§10](#10-open-questions--risks)) and the image manifest — written both **beside the output**
   (`<output>.digests.json`, for build-side auditing) **and to a defined read-only path on the
   media** (`digests.json` at the p2 mount root). This one file is the **single source of truth** used
   identically by mirroring (step 2), manifest rewriting (step 4), prepare precache
   ([§8.C](#8-work-items)), and the prepare self-check ([§7.1](#71-per-phase-success-criteria)) — so
   its schema must fully identify each image, not just carry a bare hash:

   ```json
   {
     "version": 1,
     "ociLayoutPath": "oci",
     "images": [
       { "name": "quay.io/openshift-release-dev/ocp-release",
         "digest": "sha256:9a1f...c3",
         "ociTag": "ocp-release" }
     ]
   }
   ```

   Each entry carries the **canonical image name** and its authoritative **digest**. The **OCI-layout
   locator is expressed as fields**, not a packed string: a top-level `ociLayoutPath` (relative to the
   p2 mount root, e.g. `oci`) plus a per-image `ociTag` (the layout's
   `org.opencontainers.image.ref.name`, if set); import selects by matching `digest` in the layout's
   `index.json`. Fields rather than a packed `oci:…@sha256:…` string because the `containers/image` OCI
   transport is `oci:<path>[:reference]` and does **not** accept `@sha256:`. Build, precache, and
   self-check all require this **exact set** (same count, same digests); any divergence fails the phase.
   The embedded path is recorded as `ExpectedDigestsPath` ([§8.A](#8-work-items)). (Optional, with media
   signing `--sign-key`, [§7.5](#75-media-integrity-secure-boot-signing-deferred): also emit a signed
   `media.manifest` of the immutable content-partition digests; off by default.)

### 6.5 Properties

- **Idempotent & repeatable** — same manifest ⇒ byte-reproducible-enough media; safe to re-run.
- **Image-file output by default** — produces a `.img` (dd-able) so CI/pipelines don't need a
  physical stick; writing directly to `/dev/sdX` is an opt-in.
- **Offline-verifiable** — step 6 emits the digest manifest so the bundle can be audited before
  shipping.
- **No node contact** — purely a build-side operation.

### 6.6 Open tooling questions

- Reuse `oc-mirror --v2` vs. a direct `skopeo`-based mirror in-process (fewer external deps).
- Where the RHCOS live ISO comes from in a build pipeline that may itself be disconnected.
- **`self-contained` durability — additional-image-store compatibility spike**
  ([§5.2](#52-local-resolution-and-runtime-durability), [§8.D](#8-work-items)/[§8.E](#8-work-items)):
  confirm `create-usb` can build the read-only `containers/storage` store in a driver/layer format the
  target RHCOS CRI-O reads, and that GC and reboot-time corruption cleanup leave read-only additional
  stores untouched. **If it fails, fall back to a host-level (non-CRI-O) registry** — sub-questions:
  which registry, layout→storage conversion, host-service packaging (static binary vs. podman `--root`).
  Only `install-only` avoids this — its durability is the site's external mirror.
- Media signing is **deferred and requirement-driven**, not a v1 default
  ([§7.5](#75-media-integrity-secure-boot-signing-deferred)) — Secure Boot is the primary anchor. If a
  customer mandate pulls signing in, the real unknown is **offline key distribution / trust anchor**
  (how the verifying key reaches the node's Secure Boot-validated ignition); the signing mechanism
  itself is secondary. Signing ≠ the out-of-scope at-rest encryption.

### 6.7 Example

Manifest — `sno-warehouse-01.yaml`:

```yaml
# Profile — USB installs OCP on disk; personalization comes later via the site data image
mode:            install-only

# Seed + release
seedImage:       registry.example.com/lca/seed-sno@sha256:9a1f...c3
seedVersion:     4.18.3
releaseVersion:  4.18.3

# Prepare-phase (embedded into the live-ISO ignition -> lca-cli ibi)
installationDisk: /dev/disk/by-id/wwn-0x5000c500a1b2c3d4
disconnected:     true          # zero-network image sourcing
shutdown:         true          # power off when prepare completes; ship to site

# p1 live-ISO source (verified before use; CLI --rhcos-live-iso overrides)
rhcosLiveIso:
  url:    https://mirror.openshift.com/pub/openshift-v4/x86_64/dependencies/rhcos/4.18/latest/rhcos-live.x86_64.iso
  sha256: 3b1e9c2a7f004d5e8a1c6b0f9d2e4a7c8b5f1d3e6a9c0b2d4f7a1c3e5b8d0f2a

# Optional warm-start images beyond the seed closure
# (site-only images otherwise pull from the site mirror at personalization time)
extraPrecacheImages:
  - registry.example.com/apps/telco-cnf@sha256:11aa...

# No siteConfig / clusterManifests here: personalization is delivered at the site
# as the IBI data image (virtual media), not on the USB.
```

Generate the media on a connected provisioning workstation:

`--auth-file` carries the workstation credentials used to mirror images; it is never written to
the media (see [§6.3](#63-inputs-single-manifest)).

```bash
lca-cli create-usb \
  --manifest sno-warehouse-01.yaml \
  --auth-file ~/.docker/config.json \
  --output ./sno-warehouse-01.img
```

Expected output:

```text
✓ Resolved 188 images (seed containers.list + recert + lca-cli + 1 extra), all digest-pinned
✓ Mirrored images -> OCI layout (p2 "ibi-images")        22.9 GiB
✓ Built live ISO with embedded ignition (p1)              1.2 GiB
✓ install-only: no p3 cluster-config partition (personalization via site data image)
✓ Created empty status partition (p4 "ibi-status")        16 MiB
✓ Assembled media -> ./sno-warehouse-01.img              24.1 GiB
  digest manifest: ./sno-warehouse-01.img.digests.json
  (media signing off by default; enable with --sign-key)
```

Write to a physical stick (or pass `--write-device /dev/sdX` to skip the image file):

```bash
sudo dd if=./sno-warehouse-01.img of=/dev/sdX bs=4M status=progress oflag=direct && sync
```

At the site, the box is powered on with the standard IBI **data image** attached (virtual media);
the unmodified reconfigure phase personalizes it — the USB feature is not involved in that step.

The **fully disconnected field (secondary)** profile is a different manifest: `mode: self-contained`,
`shutdown: false`, plus `clusterManifests`/`extraManifests` and `siteConfig`
(as in [§6.3](#63-inputs-single-manifest)). `create-usb` then also builds the p3 `cluster-config`
partition and the fixed on-node durability artifact (a read-only additional image store on the
installed disk + `storage.conf` drop-in; no registry, no IDMS) — and the node completes the reconfigure
phase from the still-inserted USB with no site infrastructure at all.

## 7. Success/failure detection

In zero-touch, disconnected mode there is no console, network, operator, or CR to observe. The
**authoritative truth is the exit status of each phase**; the design's job is to define what
success means per phase and deliver that status off-box.

### 7.1 Per-phase success criteria

- **Prepare** (`lca-cli ibi`, *both* profiles): success = `Run()` returns nil — disk written,
  stateroot deployed, and **every image in the profile's precache scope resident in
  `/var/lib/containers`** (seed closure + extras for `install-only`, full closure for
  `self-contained`). The image self-check is a **mandatory success gate, not optional hardening**:
  `Run()` verifies the imported image count/digests match the **on-media** digest manifest (embedded at
  `ExpectedDigestsPath`, [§6.4](#64-steps-the-tool-performs) step 6, scoped to the profile) and
  **returns an error on any mismatch or missing image**, so the phase fails *before* the terminal
  action rather than shipping an incomplete node. The `<output>.digests.json` written on the
  workstation is absent on the disconnected node, so the check reads the embedded copy.
- **Reconfigure** (`lca-cli post-pivot`, **`self-contained` only** — in `install-only` this step runs
  at the site from the existing IBI data image and is reported by that mechanism, not the USB): a
  **single** success criterion — `PostPivotConfiguration()` returns nil **and** the node reports
  `Ready` **and** all cluster operators report `Available=True`. (Node-`Ready` alone can precede
  operators settling; requiring operators `Available` is the meaningful "cluster is up" gate.) This
  same criterion is used **both** for the p4 result record and the completion signal — no looser
  definition anywhere.
  - **Clock/RTC precondition (gate, not just a risk).** recert regenerates cluster certificates at
    reconfigure time; with no NTP offline and a warehouse→ship→field time gap, an implausible RTC
    yields bad validity windows and can block `Available=True` ([§10](#10-open-questions--risks)). So
    before recert runs, reconfigure **verifies the clock is plausible** (RTC within a sane bound of
    `seedVersion`'s build/expiry, or a `ChronyConfig` local time source); if not, the phase **fails to
    the powered-on halt** with a clear `reconfigure.json` reason ([§8.F](#8-work-items)).
  - **Owner of the wait + p4 write.** Operators reaching `Available` can take many minutes after
    `PostPivotConfiguration()` returns, so the wait **must not** block inside `post-pivot`. A
    **dedicated result-writer systemd unit** (candidate host: `lca-cli init-monitor`, a new mode) polls
    the criterion with a timeout and writes `reconfigure.json` to p4 — `success` when met, `failure`
    (with the unmet condition) on timeout. Post-pivot already waits for the kube API
    (`postpivot.go:191-207`); the new unit adds node-`Ready` + operator-`Available` polling
    ([§8.H](#8-work-items)).
    - **Activation + terminal-failure propagation (against the real installed unit, retry-safe).**
      post-pivot on the installed disk runs as **`installation-configuration.service`**
      (`ExecStart=lca-cli post-pivot`, `Type=oneshot`, `RemainAfterExit=no`, `Restart=on-failure`,
      `RestartSec=5s`) — there is **no `post-pivot.service`**. The `Restart=on-failure` policy is why
      post-pivot **must not** write a failure record on each error: a *transient* failure would write
      `reconfigure.json: failure`, systemd would restart, a later attempt would succeed, and the USB
      would be **stuck showing a false failure**. So the terminal failure is written **only when the
      unit reaches `failed`** (`Restart=` attempts exhausted, start limit hit) via a systemd
      **`OnFailure=ibi-reconfigure-failed.service`** handler (a small `lca-cli` mode) that writes
      `reconfigure.json: failure` with the terminal reason and halts powered-on. `OnFailure=` fires on
      the *unit's* failed transition, not per `ExecStart` attempt, so it fires **once**, only when
      retries are genuinely exhausted — no race with the retry loop.
      The success/timeout record stays with the result-writer: **pulled into the boot target**
      (`WantedBy=multi-user.target`), ordered **`After=installation-configuration.service`**. It
      **cannot** use `Requisite=installation-configuration.service` — `RemainAfterExit=no` means a
      *successful* post-pivot goes **inactive**, so a `Requisite` would fail on the very success path it
      must handle. It instead **exits untouched if a `failure` record already exists** (the `OnFailure`
      handler got there first), else polls and writes `success` (or timeout-`failure`). The two writers
      are mutually exclusive — `OnFailure` owns the terminal-error path, the result-writer the
      success/timeout path — both tested ([§8.H](#8-work-items)).
- **`install-only` caveat:** an `install-only` success certifies *prepare only*, not a working cluster
  — the cluster is validated later, when the site personalizes the node via the existing IBI data image.

### 7.2 Result write-back (primary signal)

Each on-USB phase writes a **per-phase record** (`prepare.json`, and in `self-contained` also
`reconfigure.json` — see the contract in
[§7.4](#74-the-ibi-status-partition-contract-discovery--persistence)) to the writable `ibi-status`
partition (p4, [§4](#4-usb-media-layout)) on **both** the success and failure paths.
`ibipreparation.Run()` writes `prepare.json` before its terminal action (power-off in `install-only`,
reboot in `self-contained`); **this is the only record `install-only` produces on the USB** — its
personalization result is reported by the site data-image flow. If the `self-contained` pivot reboot
fails to transition to the installed disk, the live-ISO unit amends `prepare.json` with a
`pivot: failed` sub-status and halts powered-on ([§8.I](#8-work-items)) — so a `self-contained` stick
showing `prepare: success` with **no** `reconfigure.json` and a `pivot: failed` marker is unambiguously
a pivot failure, not a stalled reconfigure. In `self-contained`, the result-writer unit
([§7.1](#71-per-phase-success-criteria)) writes `reconfigure.json` once the success criterion is met or
on timeout; a terminal reconfigure failure (post-pivot retries exhausted) is written instead by the
`OnFailure=` handler, never by post-pivot per-error.
For example, `prepare.json`:

```json
{ "phase": "prepare", "result": "success",
  "timestamp": "2026-09-02T12:00:00Z", "seedVersion": "4.18.3",
  "checks": { "imagesExpected": 188, "imagesPresent": 188 },
  "error": null }
```

plus a compact log/journal bundle. A technician pulls the stick, mounts it on a laptop, and reads
the per-phase records — unambiguous about how far the install got, and each carries the *reason* on
failure, not just pass/fail. This is the only channel that works with no console and no network.
Writes are atomic and flushed to disk before power-off/reboot ([§7.4](#74-the-ibi-status-partition-contract-discovery--persistence)).

### 7.3 At-a-glance signal (secondary, best-effort)

To avoid pulling the stick for the common case:

- **Power-state convention:** success = powers off (`install-only` prepare) / stays up healthy
  (`self-contained` reconfigure); **failure = halt powered-on** at a rescue/emergency target. With
  this convention, "off" unambiguously means an `install-only` prepare succeeded — a failed prepare
  never powers off.
- If a serial console / BMC SOL happens to be present, print a clear `IBI SUCCESS` /
  `IBI FAILED: <reason>` banner. Cannot be relied on across Dell / HPE, so it only reinforces
  [§7.2](#72-result-write-back-primary-signal), never replaces it.

> Net-new work (item [§8.H](#8-work-items)): today `Run()` and `PostPivotConfiguration()` only return
> errors to logs a connected observer would read. Writing the result to p4 and halting (rather
> than powering off) on failure does not exist yet.

### 7.4 The `ibi-status` partition contract (discovery & persistence)

`cluster-config` (p3) has an existing discovery mechanism; `ibi-status` (p4) is new and needs its
own contract so writes are deterministic and durable:

- **Discovery.** On-USB phases locate p4 by scanning for the FS label `ibi-status` (same
  labeled-block-device approach as `cluster-config`), mount it read-write, and unmount when done.
  Prepare runs in the live ISO (both profiles). In `self-contained`, reconfigure also runs on the
  installed disk with the USB still inserted; `install-only` has no on-USB reconfigure, so p4 carries
  only `prepare.json`.
- **Missing / read-only — best-effort for `install-only`, a HARD precondition for `self-contained`.**
  The profiles differ because only `self-contained` depends on the p4 record for *safety*, not just
  observability:
  - **`install-only` (primary): observability only.** p4's absence never changes the terminal action.
    If p4 is absent or cannot be mounted read-write, prepare logs loudly and proceeds to the terminal
    action **its actual result dictates** via the [§7.3](#73-at-a-glance-signal-secondary-best-effort)
    power-state convention: a *successful* prepare still powers off; only a *failed* prepare halts
    powered-on. p4 unavailability must **not** map to `fail → halt` — that would halt an otherwise
    successful install and corrupt the very success signal §7.3 provides. Only the per-phase record is
    lost; the coarse power-state signal still reflects the real outcome.
  - **`self-contained` (secondary): p4 is a hard precondition of destructive prepare.** The re-entry
    data-loss guard ([§8.C](#8-work-items)) keys off the durable `prepare.json: success` marker, which
    **cannot** live on the installation disk — prepare wipes it. p4 is the only writable surface that
    survives the wipe, so without it the guard is blind and a re-boot would silently re-wipe a completed
    install. So before the first destructive step, `self-contained` requires
    **exactly one** writable `ibi-status` device: **fail fast (halt powered-on, no wipe)** on **0**
    (marker unreadable on re-entry) or **>1** (ambiguous which stick governs). This is the one place p4
    absence *does* map to `fail → halt`, only in `self-contained`, and `create-usb` should also flag a
    missing/duplicated p4 at build time.
- **Per-phase records, not one overwrite.** A single `status.json` cannot distinguish "prepare done"
  from "reconfigure not yet started." Write **separate per-phase records** — `prepare.json` and
  `reconfigure.json` (or a keyed object per phase) — each with its own result/timestamp, so the stick
  shows how far the install progressed.
- **Atomic & durable.** Write to a temp file, `fsync` it, `rename` into place, then `fsync` the
  directory and `sync` **before** `shutdown now` / reboot / halt — else the result can be lost in the
  buffer cache when the box powers off, defeating the primary signal.
- **p4 must be a journaled filesystem (`ext4`), not `vfat`.** The `fsync`/`rename`/`sync` sequence
  above only yields crash-safe atomic persistence on a journaled filesystem. **VFAT/FAT32 is
  non-journaled**: a power loss mid-`rename` (or after the data `fsync` but before the FAT directory
  entry lands) can leave the *only* phase-result record truncated or absent despite every
  `fsync`/`sync`. So `create-usb` **formats p4 as `ext4`** and rejects `vfat`; the atomic-write contract
  holds only against `ext4`. (Read-only p3 is never written on-node, so its FS choice is out of scope.)

### 7.5 Media integrity (Secure Boot; signing deferred)

The trust boundary for this feature is **physical possession of the stick** (a technician
hand-carries it; [§1](#1-goal)). Given that boundary, v1 leans on the integrity guarantees that
already exist or are cheap, and treats cryptographic media signing as requirement-driven hardening
rather than designed-in default. The three that carry their weight in v1:

- **UEFI Secure Boot is the primary integrity anchor** for the code that executes. Firmware
  validates the p1 boot chain (shim → GRUB → kernel/initramfs) against platform-provisioned keys (the
  RHCOS live ISO ships Red Hat-signed shim/GRUB). Verified independently of the media, so it is the
  real root of trust — not anything the USB asserts about itself. The Phase 0 spike
  ([§9](#9-phasing)) must confirm the assembled stick boots with Secure Boot enabled.
- **Verified ISO source.** `create-usb` fetches the RHCOS live ISO over **HTTPS** and verifies its
  published checksum/signature before it becomes boot media (never plain HTTP, never unverified;
  [§6.1](#61-prerequisites)). Cheap and standard, so it stays in the default flow.
- **Mandatory image self-check (completeness, not crypto).** Prepare fails unless every image in
  the on-media digest manifest is resident locally ([§7.1](#71-per-phase-success-criteria)). This
  is a *correctness* gate — it protects the make-or-break offline-resolution requirement — and is
  independent of any signing decision.

**Deferred — requirement-driven media signing (defense-in-depth).** Signing the content partitions
(p2/p3, which Secure Boot does not cover) defends only a narrow case the physical-possession model
mostly excludes — tampering with images/config *in transit* without swapping the whole stick — so it is
**not designed in by default**; add it only when a customer's supply-chain-integrity mandate requires it
(disconnected telco/gov often do). Deferring also avoids building a verification scheme atop an unsolved
primitive — **offline key distribution to the node is the real unknown**
([§6.6](#66-open-tooling-questions)). When pursued: sign a `media.manifest` of the **immutable**
partition digests (p1–p2, plus p3 in `self-contained`; never writable p4), place the detached signature
at a read-only path on p1, and have prepare/reconfigure verify it (trust-anchor key from the Secure
Boot-validated ignition) before consuming p2/p3. Captured as [§8.J](#8-work-items).

## 8. Work items

### A. Config API — `api/ibiconfig/ibiconfig.go`

- **`mode` is a `create-usb` manifest field, not an `IBIPrepareConfig` field.** The profile selector
  gates which manifest inputs `create-usb` requires ([§6.3](#63-inputs-single-manifest)); it need not
  persist into the on-node prepare config, whose behavior is fully determined by `Shutdown` (terminal
  action, §6.4 step 3 / [§8.I](#8-work-items)) and `ExpectedDigestsPath` (precache scope + self-check).
  Do not add a redundant `Mode` to `IBIPrepareConfig`.
- Add to `IBIPrepareConfig`: `LocalImagesPath string` (OCI-layout mount on USB), `Disconnected bool`,
  and `ExpectedDigestsPath string` (on-media precache-scope digest manifest, used as both the prepare
  **precache list** ([§8.C](#8-work-items)) and **self-check** reference
  ([§7.1](#71-per-phase-success-criteria))). Its scope is profile-dependent (seed closure + optional
  `ExtraPrecacheImages` for `install-only`, full site closure for `self-contained`), so
  `ExpectedDigestsPath` alone tells on-node prepare what to precache/verify — no on-node `mode` needed.
- **`install-only` (primary)** carries only prepare/image inputs + optional `ExtraPrecacheImages
  []string`; **no** durability artifact and **no** site config (both the site's, via the data image).
- **`self-contained` (secondary)** ships a **fixed** durability artifact — a read-only
  `containers/storage` additional image store + `storage.conf` drop-in (no registry, no IDMS), rendered
  by `create-usb` ([§6.4](#64-steps-the-tool-performs) step 4). No selector, no `siteMirror` option:
  the single durability model ([§5.2](#52-local-resolution-and-runtime-durability)).
- Relax `Validate()` (lines 152-183 / 224-236) so the *prepare-time* `PullSecret` may be empty or
  synthetic when `Disconnected` is set (no authenticated network registry is contacted during
  prepare). This is distinct from the *runtime* cluster pull secret, which is still mandatory for
  `self-contained` (see the manifest rules below).
- **Enforce the manifest invariants in `create-usb` validation** ([§6.3](#63-inputs-single-manifest)):
  reject `disconnected != true`; require `shutdown == true` for `install-only` and `shutdown == false`
  for `self-contained`; require a non-empty `siteConfig.pullSecret` for `self-contained` (absent for
  `install-only`). The mismatched combinations are silent-corruption risks, so they fail the build,
  not warn.
- **Provide the prepare-time auth file even when disconnected.** `IBIPrepare.Run()` still passes
  `common.IBIPullSecretFilePath` to the seed pull (`ibipreparation.go:60`) and precache
  (`ibipreparation.go:132`), so that file must exist regardless of `Disconnected`. In the
  disconnected flow the direct import from the mounted layout ([§5.1](#51-image-import-during-prepare))
  needs **no credentials**, so the prepare path writes a **synthetic auth file** — a minimal
  `{"auths":{}}` — to `IBIPullSecretFilePath` before the pull, and removes it on completion. This
  prepare-time credential is distinct from the workstation `--auth-file` (build-time only, never on
  the media) and, in `self-contained`, from the p3 runtime pull secret (`siteConfig.pullSecret`,
  applied at reconfigure) — none is interchangeable. Otherwise prepare passes the relaxed validation
  but `podman pull` fails on a missing authfile before any local image import.

### B. Local image import — new package (e.g. `lca-cli/localimages/`)

- **Prepare-time import (both flows).** Import the p2 OCI layout directly into `/var/lib/containers`
  via `skopeo`/`containers-image` (`skopeo copy oci:<layout>:<ref> containers-storage:<canonical>@sha256:D`)
  — no registry, no `registries.conf` rewriting ([§5.1](#51-image-import-during-prepare)). Imported
  images land under their **canonical** names by digest **by construction**, so the installed node
  resolves them locally without consulting any mirror on the normal path.
- **Runtime durability is a separate, profile-conditional concern
  ([§5.2](#52-local-resolution-and-runtime-durability), §8.D)** — not the import path. `install-only`
  durability is the **site's** responsibility once personalized (external mirror + IDMS via the data
  image, not the USB); only `self-contained` ships an on-node artifact (the read-only additional image
  store, distinct from the prepare-time import above). Detail in [§8.D](#8-work-items).

### C. Wire into prepare — `lca-cli/ibi-preparation/ibipreparation.go`

- Redirect the two pull sites to a **local import by digest** from the mounted layout instead of
  `podman pull`: the seed pull (line 60) and `precacheFlow` (line 73, via `podmanImgPull` /
  `workload.Precache`). Do **not** set `ReleaseRegistry`/`ReplaceImageRegistry` — that rewrites
  references to non-canonical names ([§5.1](#51-image-import-during-prepare)); the import writes
  canonical refs directly.
- **One explicit p2 lifecycle — mount once, unmount last (both flows).** p2 must stay mounted through
  the *entire* import, because the `self-contained` store handoff reads from it after precache. Ordered:
  (1) **mount p2** (OCI layout, plus read-only `store/` for `self-contained`); (2) **import + precache**
  into the installed store via chroot `/host` ([§5.1](#51-image-import-during-prepare)); (3)
  **`self-contained` only** — with the stateroot still mounted, **copy + validate** `store/` into
  `/var/lib/containers/ro-store` and install the path-rewritten `storage.conf` drop-in
  ([§8.D](#8-work-items)); (4) **`sync`, then unmount p2 last** — never before step 3, so
  `additionalimagestores` never points at a path gone with the USB.
- **Guard against a second destructive prepare on USB re-entry (data-loss prevention).** Before
  `diskPreparation()` runs `cleanupDisk()` / `coreos-installer install` (`ibipreparation.go:182-207`):
  - **`self-contained` first requires exactly one writable p4** (the hard precondition,
    [§7.4](#74-the-ibi-status-partition-contract-discovery--persistence)): halt powered-on **without
    wiping** on 0 or >1 `ibi-status` devices, since the guard below cannot otherwise function.
  - Then add a **prepare-complete check**: mount p4 by label, look for an existing `prepare.json` with
    `result: success` (the durable first-run marker, [§7.2](#72-result-write-back-primary-signal)). If
    present, this boot is a re-entry over a completed install — so **do not wipe**: amend `prepare.json`
    with `pivot: failed` and **halt powered-on** ([§8.I](#8-work-items)). This is the actual safety
    control; firmware one-time-boot selection is only advisory. Test the re-entry path (marker present ⇒
    no wipe, halt) alongside the first-run path.
- **Drive precache from the profile's declared scope, not just the seed `containers.list`.**
  `precacheFlow` currently reads only `common.ContainersListFilePath`; extend it to precache **every
  image in the on-media precache manifest** ([§8.A](#8-work-items)) — for `install-only`, the seed
  closure **+ `ExtraPrecacheImages`** (site-only images left to the site mirror at personalization);
  for `self-contained`, the **full site closure** emitted by `create-usb` alongside p2 (superset of the
  seed list, including p3-manifest/extra-manifest images). Otherwise those images stay on p2, never
  reach `/var/lib/containers`, and the `self-contained` prepare self-check fails.

### D. Persist the runtime durability artifact into the installed stateroot — `self-contained` only

- The normal path is local: canonical-by-digest images are found by CRI-O with no mirror. But a single
  cached copy is not durable (kubelet GC evicts under disk pressure; CRI-O *deletes* a corrupt image on
  reboot), so a runtime recovery source is needed. **Only `self-contained` (secondary) ships one** — the
  only profile that personalizes offline from the USB.
- **`install-only` (primary): no artifact from the USB.** Runtime durability arrives later with the
  **site's own** disconnected config (external mirror + digest IDMS) via the data image; `create-usb`
  synthesizes none. This work item does not run for `install-only`.
- **`self-contained` (secondary): read-only additional image store.** Persist a second canonical copy
  of the closure as a **read-only `containers/storage` store** on the installed disk (outside CRI-O's
  writable graphroot), registered via `additionalimagestores` in `storage.conf`. CRI-O resolves from it
  with **no registry and no IDMS**; the copy survives GC and reboot corruption cleanup (both act only on
  the writable graphroot). The build → media → installed-disk **handoff** must be explicit, since
  `create-usb` runs on the workstation and the installed stateroot does not exist until prepare writes
  the disk:
  - **Build (workstation).** `create-usb` populates the store with `skopeo copy oci:<layout>
    containers-storage:[overlay@<store>]...` and **validates** against the target CRI-O
    ([§6.4](#64-steps-the-tool-performs) step 4); matching the node's driver/layer format is a compat
    gate ([§6.6](#66-open-tooling-questions)).
  - **Carry (media).** The store ships **read-only on p2** at `store/` with its `storage.conf` drop-in
    beside it; the media is the only carrier (nothing written to the installed disk at build time).
  - **Install (prepare, in the live ISO).** Per the ordered p2 lifecycle ([§8.C](#8-work-items)), while
    the stateroot is still mounted (`/mnt`, chroot `/host`) and **before p2 is unmounted**, prepare
    copies `store/` into **`/var/lib/containers/ro-store`** (outside the writable graphroot) as
    **read-only, `root:root`**, and installs the drop-in
    **`/etc/containers/storage.conf.d/50-ibi-ro-store.conf`** with `additionalimagestores` rewritten to
    `/var/lib/containers/ro-store` (the installed-disk path — the USB mount is gone after the pivot). No
    systemd service, no registry.
- **Registry fallback only (if the compat spike fails).** A host-level (non-CRI-O) registry serving the
  on-disk copy + a **digest** IDMS → `localhost:5000` (`mirrorSourcePolicy: NeverContactSource`). Only
  here do the registry's own image, a systemd unit, and layout→registry-storage conversion apply — run
  as a *host* process outside the GC/corruption path. The IDMS must land in the
  `cluster-configuration/manifests/` set `post-pivot` applies (it runs `deleteAllOldMirrorResources`
  first).

### E. USB media creation tooling (automated) — see [§6](#6-usb-creation-tooling-automated)

- Implement `lca-cli create-usb` (per [§6](#6-usb-creation-tooling-automated)): manifest-driven,
  produces a `.img` (or writes a block device) with p1 live ISO + embedded ignition, p2 `ibi-images`
  OCI layout, an empty writable p4 `ibi-status` (result partition — see item H below), and — **only
  for `self-contained`** — a p3 `cluster-config` tree (`install-only` media omit p3 entirely;
  personalization comes from the site data image, [§4](#4-usb-media-layout)).
- **Package the `lca-cli` executable into p1** ([§6.4](#64-steps-the-tool-performs) step 3): the stock
  RHCOS live ISO has no `lca-cli` (it ships only inside the p2 runtime image), so the embedded ignition
  unit's `lca-cli ibi` would fail `command not found`. Embed the static binary via ignition (v1) and
  **test the embedded unit end-to-end with the exact binary that ships**; loading the lca-cli image from
  p2 is the rejected alternative (extra load step, container surface).
- Resolve the **profile's image scope** ([§6.4](#64-steps-the-tool-performs) step 1): reuse the seed
  `containers.list` extraction (`seedcreator.go:282`); for `install-only`, add the manifest's
  `extraPrecacheImages`; for `self-contained`, additionally scan the rendered p3 manifests /
  extra-manifests for image references. Digest-pin every one; mirror via `oc-mirror --v2` / `skopeo`;
  assemble the ISO via `coreos-installer`.
- **Rewrite shipped manifests to digest form — `self-contained` only**
  ([§6.4](#64-steps-the-tool-performs) step 4): replace every `name:tag` image reference in
  `manifests/`/`extra-manifests/` (and `SeedReconfiguration`) with its resolved `name@sha256:...`,
  and **fail the build** on any reference that cannot be pinned. (`install-only` ships no on-USB
  manifests to rewrite.)
- **Build + stage the runtime durability artifact on p2 — `self-contained` only**
  ([§6.4](#64-steps-the-tool-performs) step 4, [§8.D](#8-work-items)): a **read-only `containers/storage`
  store** (validated against the target CRI-O) + a `storage.conf` drop-in — **no IDMS, no registry**.
  Prepare performs the disk handoff ([§8.D](#8-work-items)). `install-only` emits neither.
- Emit the on-media precache-scope digest manifest that doubles as the prepare precache list and
  self-check reference ([§6.4](#64-steps-the-tool-performs) step 6, [§8.A](#8-work-items)) — seed
  closure + extras for `install-only`, full closure for `self-contained`.
- Emit a digest manifest + resulting size for auditing and size-budget validation.
- Ship user docs (a "Creating USB media" section) covering the manifest schema and usage.

### F. Reconfigure phase — `lca-cli/postpivot/postpivot.go` — `self-contained` only

- **`install-only` (primary) needs no change here.** Personalization runs at the site from the
  existing IBI **data image** (virtual media), which the unmodified `waitForConfiguration` path
  already discovers as a `cluster-config`-labeled block device
  ([§5.2](#52-local-resolution-and-runtime-durability)) — the same code, just fed by site virtual
  media instead of USB p3.
- **`self-contained` (secondary): offline reconfigure from USB p3.** Reuse the `cluster-config`
  label mechanism unchanged (p3 is the labeled device).
- Verify recert (`postpivot.go:275-292`) finds its image locally (must be precached;
  `PrecacheDisabled=false` and the recert image present in the list).
- Audit for stray network waits: chrony/NTP has no external server offline (rely on RTC /
  local config via `ChronyConfig`); DNS is local via dnsmasq — both are already local-friendly.
- **Add the clock/RTC precondition before recert** ([§7.1](#71-per-phase-success-criteria)): check the
  system clock is plausible (RTC within a sane bound, or a `ChronyConfig` local time source present)
  and **fail to the powered-on halt** with a clear `reconfigure.json` reason if not, so recert never
  mints certificates with a bad validity window offline. Test both the pass and the skewed-clock path.

### G. Warehouse shutdown (`install-only` terminal action)

- Reuse `IBIPrepareConfig.Shutdown`: after a successful `install-only` prepare the node powers off
  for shipping. Documentation only.

### H. Success/failure detection — see [§7](#7-successfailure-detection)

- Add the writable `ibi-status` partition (p4) to the media layout and creation tool ([§6](#6-usb-creation-tooling-automated)).
- Implement the p4 contract ([§7.4](#74-the-ibi-status-partition-contract-discovery--persistence)):
  discover by FS label, per-phase records, atomic write + `fsync`/`sync` before power-off/reboot/halt,
  graceful degrade when p4 is missing/read-only.
- Write the `prepare.json` record + log bundle from `ibipreparation.Run()` (both success and failure
  paths, before the terminal action). **This is the only p4 record `install-only` produces** — its
  personalization success is reported later by the site data-image flow.
- **`self-contained` only** — add a **dedicated result-writer systemd unit** (candidate host:
  `lca-cli init-monitor`, today an IBU auto-rollback monitor — a new mode) that polls the reconfigure
  success criterion with a timeout and writes `reconfigure.json` to p4, so the long operators-`Available`
  wait does not block `post-pivot`. Activation, ordering against the real
  `installation-configuration.service`, and the p4-record gate (not a systemd `Requisite`) are specified
  in [§7.1](#71-per-phase-success-criteria); test both the success and the post-pivot-error outcomes.
- On failure, **halt powered-on** (rescue/emergency target) instead of powering off, so the
  `install-only` success signal (power-off) is unambiguous.
- Make the prepare self-check (imported image digests vs. on-media manifest) a **mandatory gate**
  that fails `Run()` before the terminal action. In `self-contained`, also add the single reconfigure
  success criterion (node `Ready` + cluster operators `Available`), used identically for the p4
  result record and the completion signal ([§7.1](#71-per-phase-success-criteria)).

### I. On-USB pivot reboot — live-ISO ignition unit — `self-contained` only

- The unit that runs `lca-cli ibi` must, on successful return with `Shutdown: false`, issue the
  reboot into the installed disk so on-USB reconfigure/post-pivot then runs. `install-only` uses
  `Shutdown: true` — it powers off after prepare with no on-USB pivot (the site boots the installed
  disk later), so this item is **`self-contained` only** and the two terminal actions are mutually
  exclusive. Today nothing owns the reboot because IBIO is bypassed (see
  [§2](#2-how-todays-ibi-maps-onto-the-two-usb-flows)).
- **Define the pivot as prepare's terminal action, and the failure state if it does not take.** The
  reboot is issued *after* `prepare.json: success` is flushed to p4. If the `reboot` command returns
  (or the node comes back up still in the live ISO rather than on the installed disk), the unit must
  **not idle in the live ISO** — it records an explicit pivot-failure marker (amends `prepare.json`
  with a `pivot: failed` sub-status, [§7.2](#72-result-write-back-primary-signal)) and **halts
  powered-on** at the rescue target, exactly like any other prepare failure
  ([§7.3](#73-at-a-glance-signal-secondary-best-effort)). This keeps the coarse power-state signal
  honest (a healthy `self-contained` node is up *on the installed disk*; a node stuck powered-on in the
  live ISO is a failure) and lets a technician distinguish "prepare succeeded but pivot failed" (marker
  present, no `reconfigure.json`) from a reconfigure failure (post-pivot's own `reconfigure.json:
  failure`).
- Add a `self-contained` test covering the live-ISO → installed-disk transition, that `post-pivot`
  then runs, **and** the pivot-failure path (reboot fails ⇒ marker written + powered-on halt).

### J. Media integrity & threat model — see [§6.1](#61-prerequisites) / [§7.5](#75-media-integrity-secure-boot-signing-deferred)

**In v1 (designed in):**

- `create-usb` fetches the RHCOS live ISO over HTTPS and verifies its published checksum/signature
  before it becomes boot media.
- Rely on UEFI Secure Boot as the primary integrity anchor for the executing code (validated in the
  Phase 0 spike) and on the mandatory image self-check ([§7.1](#71-per-phase-success-criteria)) for
  offline completeness.
- Document the physical-possession-trusted threat model for the unencrypted p3 pull secret
  (`self-contained` only; `install-only` carries no site pull secret) ([§1](#1-goal)).

**Deferred (requirement-driven, off by default — [§7.5](#75-media-integrity-secure-boot-signing-deferred)):**

- Optional `create-usb --sign-key`: sign a `media.manifest` of the **immutable** partition digests
  (p1–p2, plus p3 in `self-contained`; always excluding writable p4), detached signature at
  `/ibi/media.manifest`(`.sig`) on p1.
- Solve **offline key distribution** to the node (the real unknown; [§6.6](#66-open-tooling-questions))
  and have prepare/reconfigure verify the signature + partition digests before consuming p2/p3.
- Pursue only when a customer supply-chain-integrity mandate requires it; not a prerequisite for the
  core flow.

## 9. Phasing

0. **Phase 0 — bootability spike (gate).** Prove one single-stick assembly (p1–p4) boots on
   reference Dell / HPE GNR-D hardware with UEFI Secure Boot enabled, and that the live environment
   enumerates p2/p3/p4 when booted from p1 ([§4](#4-usb-media-layout), [§10](#10-open-questions--risks)).
   **Gates the media contract** and the assembly step ([§6.4](#64-steps-the-tool-performs) step 5);
   until it passes, the four-partition layout is provisional. May run in parallel with Phase 1.
1. **Phase 1 — core image sourcing (A–D) + warehouse shutdown (G) + on-USB pivot reboot
   (I, `self-contained`).** Two exit criteria, one per profile:
   - **`self-contained` (secondary, strictest):** a node prepared with the NIC physically unplugged,
     the full closure resident in `/var/lib/containers` under canonical names, that boots (on-USB
     pivot reboot) and reaches a running cluster with the NIC still unplugged — then force-evict/corrupt
     an image and confirm on-node recovery from the read-only additional image store (no re-pull, no
     registry; the registry fallback, if used, must also recover with the NIC unplugged).
     - **Clock/RTC gate (pass path,
       [§7.1](#71-per-phase-success-criteria)/[§8.F](#8-work-items)):** with a plausible RTC (or a
       `ChronyConfig` local time source), reconfigure passes the pre-recert clock check, recert mints
       certificates with valid windows, and operators reach `Available=True` so the result-writer
       records `reconfigure.json: success`.
   - **`install-only` (primary):** a node prepared offline (seed closure + extras resident in
     `/var/lib/containers` under canonical names), powered off, then **personalized at a
     mirror-connected site via the existing IBI data-image (virtual media) mechanism** — no USB
     personalization. Confirm the installed disk boots and reconfigures from the site data image,
     seed-closure images resolve from the precache with no pull, and any site-only image pulls from
     the **site's own** mirror via the site's standard disconnected config (the USB ships no IDMS).

   (Media hand-assembled from a script for this phase; the automated tool is Phase 2.)
2. **Phase 2 — automated USB creation tooling (E / [§6](#6-usb-creation-tooling-automated)), config delivery validation (F),
   success/failure detection (H / [§7](#7-successfailure-detection)), media integrity (J, v1 scope).** Requires Phase 0's
   validated layout. Exit criterion: `lca-cli create-usb <manifest>` produces bootable media
   (Secure Boot verified, ISO checksum verified) consumed end-to-end by Phase 1's flow, and each run
   writes readable per-phase results to p4. (Optional media signing is deferred/requirement-driven —
   [§7.5](#75-media-integrity-secure-boot-signing-deferred).)
3. **Phase 3 — end-to-end `install-only` (primary) + fully disconnected `self-contained` (secondary)
   flows, broader Dell / HPE GNR-D hardware validation, USB size budgeting.** Includes the full
   `install-only` warehouse-install → ship → **standard IBI data-image personalization at a
   mirror-connected site** path (durability from the site's own mirror), and the `self-contained`
   flow personalized entirely from the USB with no mirror at all.
   - **Clock/RTC gate (fail path,
     [§7.1](#71-per-phase-success-criteria)/[§8.F](#8-work-items)):** with the RTC deliberately skewed
     to an implausible time and no local time source, `self-contained` reconfigure **fails the
     pre-recert clock check**, recert never runs, and the phase writes a `reconfigure.json` failure
     reason and **halts powered-on** — no certificates minted with a bad validity window.

## 10. Open questions / risks

- **Digest vs. tag resolution offline ([§5.2](#52-local-resolution-and-runtime-durability))** — the make-or-break correctness point; needs a
  disconnected e2e test confirming every image resolves locally under its canonical name by digest
  (normal path needs no mirror) and that no stray runtime **tag** reference exists — the shipped
  mirror is a digest IDMS, so a tag reference has no offline resolution.
- **Runtime image durability & recovery ([§5.2](#52-local-resolution-and-runtime-durability))** — a
  single cached copy is not durable (GC eviction; CRI-O deletes a corrupt image on reboot). Recovery is
  profile-conditional: **primary** relies on the site's own disconnected config (external mirror +
  digest IDMS via the data image); **secondary** carries a read-only additional image store on disk (no
  registry, no IDMS). Each needs an e2e test that evicts/corrupts an image and confirms recovery
  (`install-only`: site mirror, no internet; `self-contained`: NIC unplugged). Whole-disk failure is out
  of scope (re-provision).
- **Site personalization contract (`install-only`)** — the USB delegates personalization and runtime
  durability to the site's existing IBI data-image flow. Confirm the site's disconnected config
  (external mirror + IDMS, pull secret, site manifests) holds the full closure the node references; the
  USB precaches only the seed closure + extras, so any site-only image absent from the site mirror has
  no source.
- **USB size budget** — the release payload plus operators can be tens of GB; fits on modern
  USB but must be stated in docs and validated against target media.
- **Live-ISO ignition ownership** — today the external image-based-install-operator (IBIO)
  builds the config ISO; USB deliberately bypasses IBIO (no BMC/VirtualMedia), so lca-cli/LCA
  must own USB + ignition assembly. Confirm this ownership boundary with the IBIO team.
- **Prepare-time auth file** — the direct import needs no credentials, so prepare writes a synthetic
  `{"auths":{}}` authfile ([§8.A](#8-work-items)); confirm relaxed `Validate()` and synthetic-authfile
  handling versus any residual code path that still expects a real pull secret during prepare. (The
  runtime cluster pull secret is separate: in `install-only` it is the site's, applied during
  data-image personalization; in `self-contained` it is the p3 `siteConfig.pullSecret`.)
- **USB creation tooling ([§6.6](#66-open-tooling-questions))** — `oc-mirror` vs. in-process `skopeo`; live-ISO source in a
  disconnected build pipeline; optional media signing.
- **Media assembly mechanics** — *format collision.* An RHCOS live ISO is an **isohybrid**
  image: one file that is both an ISO9660 filesystem and a self-contained bootable disk image
  with its own partition table + EFI System Partition. `dd`-ing it defines the stick's *entire*
  partition table, leaving no room for p2 (~20–30 GB of images) or p3. So placing p2/p3 on the
  same stick means **editing the GPT the ISO created**, and whether the result still boots is
  firmware- and Secure-Boot-dependent. Candidate layouts, each with an unknown to resolve by a
  spike on Dell / HPE GNR-D:
  - **(a) Append after the isohybrid ISO** — `dd` the ISO, grow the GPT, add p2/p3 in free
    space. Precedent exists (RHCOS live-ISO persistent partition), but editing the hybrid
    GPT/MBR can break UEFI Secure Boot / BIOS fallback on some firmware.
  - **(b) Build a fresh partitioned image** — GPT with a signed ESP + extracted live-ISO boot
    contents on p1, plus p2/p3. Full control, but re-implements isohybrid (shim/GRUB signing
    for Secure Boot must be correct).
  - **(c) Embed images/config as files inside the ISO** — no p2 partition; simplest partition
    story, but ISO9660 is read-only and ~30 GB inside an ISO is untested at that scale, **and**
    post-pivot still needs `cluster-config` as a *labeled block device*
    (`waitForConfiguration`), so p3 likely must remain a real partition regardless.

  Spike must answer, on reference hardware: boots with Secure Boot enabled? live environment
  enumerates p2/p3 when booted from p1? which of (a)/(b)/(c)? The choice drives both [§4](#4-usb-media-layout) (layout)
  and [§6.4](#64-steps-the-tool-performs) step 5 (assembly).
- **Clock/RTC accuracy offline** — recert regenerates cluster certificates at reconfigure
  (site-activation) time. With no NTP and a warehouse→ship→field time gap, an inaccurate RTC
  could yield bad certificate validity windows. This is now handled as an **explicit reconfigure
  gate** ([§7.1](#71-per-phase-success-criteria), [§8.F](#8-work-items)): reconfigure verifies a
  plausible clock (RTC bound, or a `ChronyConfig` local time source) before recert and halts on
  failure. Open sub-question: the exact plausibility bound and whether a local time source is
  mandated per deployment.
