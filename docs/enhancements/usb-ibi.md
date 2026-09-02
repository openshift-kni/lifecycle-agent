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

- **Warehouse pre-install (primary) — install-only USB.** The USB does only the **installation**
  half of IBI: write the seed/OCP image to disk and precache images from the stick, offline at the
  warehouse, then power off and ship. **Personalization happens later, at a mirror-connected site,
  through the existing IBI data-image mechanism** — the site's config ISO ("data image") mounted via
  virtual media, consumed by the unmodified reconfigure path. The site's external mirror is the
  runtime image source; the on-USB image store is prepare-time only, the SNO runs no registry of its
  own, and the USB carries **no site config and synthesizes no mirror artifact** — that all comes from
  the site's standard disconnected config.
- **Fully disconnected field deployment (secondary) — self-contained USB.** With no site
  infrastructure ever (no BMC, no virtual media, no mirror), the USB installs *and* personalizes
  offline and the node **stays offline forever**. It carries the full four-partition payload
  (including on-USB site config) and its own runtime recovery source for images.

Scope is SNO only. The USB *install* step requires no BMC/virtual media in either profile; the
primary profile's *personalization* then leverages the connected site's standard IBI capabilities.

**Most of the flow already exists.** Installation is already a two-phase process — a *prepare* phase
that runs from bootable install media, and a *reconfigure* phase that runs on first boot of the
installed system. The primary profile uses **only** the prepare phase from USB and hands the
reconfigure phase to the existing site mechanism; the secondary profile drives both from USB. Two
needed behaviors already work with little or no change: **site config delivery** (the reconfigure
phase already reads config from an attached, labeled storage device — exactly the data-image path the
primary profile relies on) and **power off after prepare** (the warehouse behavior).

**The core problem is sourcing images offline during install.** Today all images are pulled over the
network. The central new capability is importing images from the USB into the system's image store
**under their canonical names by digest** ([§5.1](#51-image-import-during-prepare)) so the installed
system resolves them locally. **Runtime image durability then depends on the profile**
([§5.2](#52-local-resolution-and-runtime-durability)): images can be evicted by
garbage collection or wiped on reboot if corrupted, and something must restore them. In the
**primary** profile that source is the **external site mirror**, configured by the site's standard
data image (a digest `ImageDigestMirrorSet`) — the USB is not involved. Only the **secondary**
(forever-offline) profile must carry its own on-node recovery source — a read-only additional image
store shipped on the disk (no registry, no mirror).

**What the USB carries depends on the profile:** the primary (install-only) USB carries the boot
environment, the images, and a writable results area (three areas); the secondary (self-contained)
USB adds the site configuration (four areas). The secondary profile's USB stays inserted through
first boot; both require a technician to force a one-time boot from the USB (the subsequent reboot
must land on the installed disk).

**Automated USB creation is an in-scope deliverable:** a tool that runs on a connected provisioning
workstation and turns a single input manifest into ready-to-boot media — resolving the image set,
mirroring images onto the stick, building the boot environment with the install config embedded,
(for the secondary profile) rendering the site config, assembling the media, and emitting an
auditable list of what was included.

**Success detection without a console or network:** the authoritative signal is whether each phase
succeeded, delivered two ways — a result file plus logs written to the USB's writable area (read
off-box by a technician), and a power-state convention (success powers off or stays healthy; failure
halts powered-on). The install is binary — no rollback, so remediation is re-run or re-image. The
primary profile's USB reports only the prepare (install) result; personalization success is reported
by the existing site mechanism.

**Bottom line:** install flow, config delivery, and shutdown are largely already there. The genuinely
new work is offline image sourcing at install time and the automated USB creation tool (both
profiles), plus — **secondary profile only** — the on-USB reconfigure/field-flow reboot and a runtime
durability artifact (a read-only additional image store on disk). Putting a bootable install image
and large data partitions on one stick under Secure Boot is the main unknown to resolve first.

## 1. Goal

Enable IBI to **install with zero network connectivity** using bootable USB media. The install is
offline in both profiles; they differ in how much of IBI the USB owns and in what the node has at
run time:

1. **Warehouse pre-install (primary) — install-only:** power on → boot USB → **prepare phase**
   (write OCP/seed to disk + precache images) completes → the system shuts down → ship to site. At the
   site the node is **personalized later using the existing IBI data-image mechanism** (the config ISO
   mounted via virtual media, consumed by the unmodified reconfigure phase). The site has a **mirror
   registry external to the SNO**; at runtime the node is a standard mirror-connected SNO. The USB
   carries no site config.
2. **Fully disconnected field deployment (secondary) — self-contained:** power on → boot USB → **both
   phases run from USB** (prepare → pivot → reconfigure) → node is operational in a **fully
   disconnected environment and stays offline for life** — no upstream or mirror ever, so the USB
   carries the site config and the node carries its own runtime image recovery source.

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
> inserted stick and **no BMC/RedFish virtual media** in either profile — that is the offline-install
> requirement. This does **not** forbid virtual media at a connected site: the primary profile's
> *personalization* deliberately reuses the existing IBI data-image path, where the site delivers the
> config ISO by virtual media as it does for standard IBI. The constraint is on the USB install, not
> on the site's later personalization.

> **Threat model — physical possession of the USB is trusted.** Because at-rest encryption is out of
> scope, any site config **on the USB is unencrypted**. This only applies to the **secondary**
> profile, whose USB carries p3 (`cluster-config`) with the **cluster runtime pull secret
> (`siteConfig.pullSecret`)** in the clear — anyone who obtains that stick can read it. The design
> assumes the secondary USB is handled as a trusted, controlled item through warehouse → ship → field,
> equivalent to handing over a machine pre-loaded with credentials; if that does not hold, the pull
> secret must be provisioned through a protected activation step instead of baked into p3. The
> **primary** profile carries no site config on the USB, so this credential-confidentiality concern
> does not apply to it (its pull secret arrives in the site data image via the existing mechanism).
> Media *integrity/authenticity* of the executing code rests on UEFI Secure Boot; optional media
> signing for the content partitions is deferred and requirement-driven
> ([§7.5](#75-media-integrity-secure-boot-signing-deferred)).

> **Scope note:** the original feature request listed an automated USB-creation pipeline as out
> of scope (documented manual process only). That has been **brought in scope** for this
> effort — **automated USB creation tooling is a deliverable** (see [§6](#6-usb-creation-tooling-automated)). A repeatable,
> parameterized tool is required, not a runbook.

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
> otherwise **returns without rebooting** — there is no reboot in the prepare path today. In the
> standard IBI flow the external image-based-install-operator (IBIO) owns the post-prepare reboot;
> the USB flow deliberately bypasses IBIO (no BMC/VirtualMedia), so nothing currently transitions
> the field flow from the live ISO to the installed disk. This feature must define that owner: the
> live-ISO ignition unit that runs `lca-cli ibi` **issues the pivot reboot after prepare returns
> successfully when `Shutdown` is false** (warehouse `Shutdown: true` powers off instead — the two
> are mutually exclusive terminal actions). This is tracked as work item
> [§8.I](#8-work-items) and needs a field-flow test covering the live-ISO→installed-disk transition.

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

This same discovery path is what the **primary (install-only) profile** relies on at the site: the
`cluster-config` device it finds there is the site's **data image** (config ISO) mounted via virtual
media, not a USB partition. The reconfigure code does not care which — it discovers the label, mounts,
and copies — so the primary profile's personalization is the **unmodified existing mechanism**. Only
the **secondary** profile supplies `cluster-config` as an on-USB partition (p3).

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

> Note: shutdown-on-completion exists **only** for the prepare phase. There is no
> shutdown-after-reconfigure path (`PostPivotConfiguration` ends with `cleanup()` and boot
> continues), and the reboot client (`internal/reboot/reboot.go`) only reboots, never powers
> off. The field flow wants the node *operational* after reconfigure, so this is fine —
> but if a "reconfigure then power off" mode is ever requested it would be new work.

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
> live ISO can coexist with the p2/p3/p4 data partitions on one stick — under UEFI Secure Boot and
> BIOS fallback on reference firmware — is unresolved (the format-collision problem in
> [§10](#10-open-questions--risks)). A **bootability spike covering all of p1–p4 on reference Dell /
> HPE GNR-D hardware is a gate** that must pass before the media contract here (and the assembly in
> [§6.4](#64-steps-the-tool-performs) step 5) is finalized. If no single-stick assembly proves
> bootable, the layout changes (e.g. images move inside the ISO, or a two-stick fallback), so treat
> the partition table below as the intended design, not a settled contract.

| Part | FS / type | Label | Contents | Consumed by |
|------|-----------|-------|----------|-------------|
| p1 | ISO9660 / EFI (El Torito) | — | RHCOS live ISO + embedded ignition that auto-runs `lca-cli ibi -f <cfg>` | firmware boot → live environment |
| p2 | xfs/ext4 (may be read-only) | `ibi-images` | OCI image layout: seed image + **all** referenced container images (release payload, operators, recert, lca-cli) | **prepare** — mounted, then imported into `/var/lib/containers` under canonical names ([§5.1](#51-image-import-during-prepare)) |
| p3 (**secondary only**) | ext4/vfat | `cluster-config` | `/opt/openshift/...` reconfigure tree (`SeedReconfiguration`, `manifests/`, `kubeconfig-crypto/`, net config, `extra-manifests/`) | **reconfigure** — found by FS label (primary: same device arrives as the site data image) |
| p4 | ext4/vfat (**writable**) | `ibi-status` | per-phase records (`prepare.json`; `reconfigure.json` self-contained only) + logs | **prepare**; **reconfigure** (self-contained) — found by FS label; atomic writes ([§7.4](#74-the-ibi-status-partition-contract-discovery--persistence)) |

The **prepare config** itself (`IBIPrepareConfig`: `SeedImage`, `InstallationDisk`,
`Shutdown`, and the new `LocalImagesPath`/`Disconnected`/`ExpectedDigestsPath` fields from Work items
[§8.A](#8-work-items)) is embedded in the p1 live-ISO **ignition** rather than placed on its own
partition — this keeps the flow zero-touch (no extra mount, config travels with the boot media). This
is identical for both profiles; the profile difference is only whether p3 is present on the USB.

Why these, specifically:

- **p1** is standard `coreos-installer` output; LCA does not build the live ISO today
  (`RHCOSLiveISO` is normally a `mirror.openshift.com` URL), so USB creation tooling ([§6](#6-usb-creation-tooling-automated))
  must produce it and embed the auto-run ignition.
- **p2** is the whole point of [§5](#5-the-core-problem-zero-network-image-sourcing) — the offline
  image source. Its discovery/mount contract mirrors p3/p4's labeled-block-device approach: the
  live-ISO prepare unit locates p2 by the FS label `ibi-images` (**not** a hard-coded device node),
  mounts it **read-only** at a defined path, and sets `LocalImagesPath` to that mount **before**
  `lca-cli ibi` runs. Discovery is a **hard precondition** for prepare (unlike p4, which is
  best-effort): if no `ibi-images` device is found, or **more than one** matches, `create-usb`'s
  embedded unit **fails fast before any disk write** rather than proceeding to a network pull — there
  is no network to fall back to ([§7.1](#71-per-phase-success-criteria)). The mount is released after
  precache completes ([§8.C](#8-work-items)).
- **p3 (secondary only)**'s label is **not arbitrary**: `post-pivot`'s `waitForConfiguration`
  (`postpivot.go:912-944`) scans block devices for exactly the `cluster-config` label
  (`seedreconfig.go:8`). Using this label means the existing mount-and-copy code consumes the
  partition with **zero change** (see [§3.1](#31-config-delivery-by-labeled-block-device-already-works)).
  The primary profile ships no p3 — the same `cluster-config` device is provided at the site as the
  data image (virtual media), consumed by the identical code path.
- **p4** is the only *writable* area on the media: in zero-touch disconnected mode there is no
  console or network, so each phase writes its result marker + logs here for a technician to read
  off-box (rationale and format in [§7](#7-successfailure-detection)). It must be created empty by the tool ([§6](#6-usb-creation-tooling-automated)) so the
  first write at prepare/reconfigure time has a target.

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

> **Note — the technician must force the initial boot from the USB.** The prepare phase only
> starts if firmware boots the USB's live ISO (p1). The technician selects the USB as a
> **one-time boot device** (UEFI/BIOS boot menu) or sets it first in the boot order; do not rely
> on default boot order, since the `InstallationDisk` may already carry a bootable OS. This is a
> **one-time** action: the pivot reboot after prepare must land on the *installed disk*, **not**
> re-boot the USB. If the USB were forced again (e.g. left first in a persistent boot order),
> the node would re-run prepare and wipe the freshly installed disk. Use one-time boot selection,
> or ensure the installed disk precedes the USB in the persistent boot order for the second boot.

Implications:

- No code is needed to copy the config onto the installed disk before reboot; the existing
  labeled-block-device discovery works as-is (from USB p3 for secondary, from the site data image for
  primary).
- For the secondary profile, the removable USB must remain enumerable as a block device after the
  pivot reboot (true for a physically inserted stick).

## 5. The core problem: zero-network image sourcing

This is the one hard requirement with **no existing support**. Today every image path in LCA
is a network `podman pull`:

- Seed image: `ibipreparation.go:60` — `podman pull --authfile <IBIPullSecretFilePath> <SeedImage>`.
- Precache: `internal/precache/workload/pullImages.go:57` — `podmanImgPull` is literally
  `podman pull <image> --authfile <file>`.

There is **no `oci:` / `dir:` / `containers-storage:` / local-registry transport** anywhere in
first-party code. The only "local" concepts are `ostree pull-local` (OS filesystem, not
container images) and `podman image mount` of an already-pulled seed.

What *does* exist is a **registry-hostname remap** path —
`IBIPrepareConfig.ReleaseRegistry` + `ShouldOverrideSeedRegistry` + `ReplaceImageRegistry`
(`utils/client_helper.go:275-425`, `utils/utils.go:263`) — plus `registries.conf` parsing
(`ibipreparation.go:238-258`, using `containers/image/v5/pkg/sysregistriesv2`), and the
`ImageDigestSources { Source, Mirrors }` config shape (`ibiconfig.go:137-143`). Note the remap path
is **not** reusable here: `ReplaceImageRegistry` *rewrites* the reference hostname, so it stores
images under non-canonical names — the defect that rules out reusing it for local import
([§5.1](#51-image-import-during-prepare)). The design instead adds a `containers-storage:` import
path.

### 5.1 Image import during prepare

Prepare must leave **all required images resident in the persistent `/var/lib/containers` partition,
stored under their canonical names by digest** — the property
[§5.2](#52-local-resolution-and-runtime-durability) depends on for both flows.
Storage *naming* is the make-or-break detail: an image stored under its canonical name
(`quay.io/…@sha256:…`) is the one a pod resolves locally with no mirror, and the one a runtime IDMS
recovers by redirecting canonical → mirror. How images get there during prepare is a separate,
smaller question from the runtime durability artifact (external site mirror for primary vs on-node
registry for secondary — [§5.2](#52-local-resolution-and-runtime-durability)).

> **What the precache set covers differs by profile.** Today `precacheFlow` imports only the seed's
> `containers.list` (`common.ContainersListFilePath`). The on-media precache list is emitted by
> `create-usb` ([§6.4](#64-steps-the-tool-performs) step 1) and prepare drives from **that list, not
> the seed list alone** (work item [§8.C](#8-work-items)). Its scope depends on the profile:
>
> - **Primary (install-only):** the **seed closure** — seed `containers.list` + recert + lca-cli —
>   **plus any optional extra images** the operator names in the manifest. Site-specific images
>   referenced only by the (site-delivered) data image are **not** on the USB and are **not required
>   offline**: at the mirror-connected site they pull from the external mirror. So the primary
>   precache is a *warm-start optimization*, and its self-check ([§7.1](#71-per-phase-success-criteria))
>   asserts the seed closure + declared extras are resident, not a full site closure.
> - **Secondary (self-contained):** the **full closure** — seed list + recert + lca-cli **plus every
>   image referenced by the shipped p3 manifests/extra-manifests** — because a forever-offline node
>   can never pull anything. Every image in that closure must be imported into `/var/lib/containers`
>   during prepare; the self-check fails if any is missing, and first pod start must not block on a
>   pull.

#### Chosen — direct `containers-storage` import

1. Mount the USB OCI layout (p2, [§4](#4-usb-media-layout)).
2. For the seed and every image in the closure, import it directly into the target container store
   **under its canonical name by digest**:
   `skopeo copy oci:<usb-layout>:<ref> containers-storage:[overlay@/var/lib/containers+...]<canonical>@sha256:D`.
3. **The target store is the *installed* stateroot's `/var/lib/containers`, not the live-ISO's
   ephemeral store** — otherwise the imported images vanish at the pivot. This reuses the mechanism
   the existing precache flow already relies on: prepare sets `OstreeDeployPathPrefix=/mnt/` and
   deploys the stateroot on the mounted install disk (`ibipreparation.go:66-71`), and `precacheFlow`
   **chroots to `/host`** (the mounted installed root) before `workload.Precache`
   (`ibipreparation.go:122-132`), so writes land in the persistent on-disk store. The new
   `containers-storage` import targets that **same** store (via the chroot, or an explicit
   `overlay@<installed-root>/var/lib/containers` graphroot), so images imported during prepare are the
   images CRI-O finds after first boot.

Because the destination reference is canonical, storage is canonical **by construction** — no
registry, no `registries.conf`, no reference rewriting. This satisfies
[§5.2](#52-local-resolution-and-runtime-durability) identically for both profiles:
the normal path is a local hit under the canonical name. Recovery of a GC-evicted or corruption-wiped
image then differs by profile: the **primary** profile re-pulls from the site mirror via the site's
runtime IDMS (canonical → site mirror, supplied by the data image), while the **secondary** profile
finds the image still present in its read-only additional image store under that *same* canonical name
— **no re-pull and no IDMS**. The prepare-time cost is a new local-import path at the two pull sites —
`podmanImgPull` (`pullImages.go:57`) and the seed pull (`ibipreparation.go:60`) — which must target
the mounted store by digest instead of running `podman pull`
([§8.B](#8-work-items)/[§8.C](#8-work-items)). The import mechanism is decoupled from the runtime
durability artifact: the primary profile's IDMS comes from the site (not the USB), and the secondary
profile's durable copy (the read-only additional store) is built from the on-disk layout
([§5.2](#52-local-resolution-and-runtime-durability), [§8.D](#8-work-items)) — not
the thing that served prepare.

> **Rejected — local registry + `ReleaseRegistry` remap.** The tempting shortcut is to run a
> `localhost:5000` registry over the layout and reuse the existing `ReleaseRegistry` override so
> precache "just works" unchanged. It does not. `ReleaseRegistry` drives `ReplaceImageRegistry`
> (`utils/utils.go:263`), a regex that **rewrites the reference hostname** — so images pulled through
> it are stored under `localhost:5000/…`, *not* their canonical names. That breaks the no-mirror
> normal path ([§5.2](#52-local-resolution-and-runtime-durability)) and, worse,
> strands the **primary** flow outright: a runtime IDMS mapping canonical → the external site mirror
> matches nothing in a `localhost:5000`-named store, so every image re-pulls from the site mirror at
> first boot and the precache is dead weight. Rebuilding this approach to store canonically would
> require a genuine `registries.conf` digest-mirror (canonical → localhost, `pull-from-mirror =
> "digest-only"`) plus pulling canonical refs — strictly more moving parts than the direct import,
> with no advantage. Its one apparent benefit — reusing the `ReleaseRegistry` override — is precisely
> the defect.

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
  **image garbage collection** (unused images are evicted under disk pressure, above
  `imageGCHighThresholdPercent`) and to ordinary on-disk corruption. Worse, **CRI-O removes corrupted
  images on reboot** — its storage integrity check wipes invalid images so they re-pull on next use —
  so a corrupt image is not merely unreadable, it is *deleted* on the next boot. On a connected node
  this is harmless: the image re-pulls. So the design needs a **recovery source** for evicted or
  corruption-wiped images — and *where that source lives is what differs between the two flows.*

**Primary profile — recovery is the site's job, not the USB's.** The warehouse-prepared node
activates at a site with a **mirror registry external to the SNO**, so this is a standard
mirror-connected SNO at run time. The digest **`ImageDigestMirrorSet`** mapping each canonical source
registry → the external site mirror arrives in the **site's standard disconnected config, delivered
as the data image via the existing mechanism** ([§3.1](#31-config-delivery-by-labeled-block-device-already-works))
— **`create-usb` does not synthesize or ship it**. Normal resolution stays local (images precached
under canonical names, [§5.1](#51-image-import-during-prepare)); the site IDMS's job is **recovery,
not initial resolution**: when an image is GC-evicted or wiped on reboot after corruption, CRI-O's
standard pull path re-fetches it from the site mirror — **automatic across a reboot**, with no
upstream internet, exactly how disconnected OpenShift already works. **The SNO runs no registry of its
own**, the on-USB OCI layout is a *prepare-time only* image source, and image durability is entirely
out of the USB feature's scope for this profile. The USB's only obligation is **canonical storage**
([§5.1](#51-image-import-during-prepare)) so that the site IDMS resolves.

**Secondary profile — recovery must live on the node, shipped by the USB.** A forever-offline node
has **no external mirror ever** and no site data image, so the recovery source has to be carried on
the node itself and shipped by `create-usb`. The mechanism is a **persistent read-only additional
image store**: `create-usb` writes a second, canonical copy of the closure into a **read-only
`containers/storage` store** on the installed disk (outside CRI-O's writable graphroot) and registers
it via `additionalimagestores` in `storage.conf`. CRI-O then resolves every closure image from that
store by canonical name/digest with **no registry and no mirror on any path** — initial resolution and
recovery alike. This survives both failure modes automatically and across a reboot: kubelet image GC
evicts only from the *writable* graphroot, and CRI-O's reboot-time corruption cleanup deletes only
from the *writable* store — the read-only additional-store copy is untouched in both cases, so the
image is still present and **no re-pull is needed**. Cost: roughly **2×** the image footprint on disk
(the writable working copy plus the durable read-only copy) and **no long-running service**. This is
the single durability model for the secondary profile; it needs no on-node registry and no IDMS.

**Why not an on-node registry (bootstrap circular dependency).** An earlier design ran a persistent
`localhost:5000` registry over the on-disk copy plus a digest IDMS redirecting canonical →
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
not hold, the fallback is an on-node registry **run outside CRI-O** — a host-level systemd service
(a static registry binary in the stateroot, or podman with a **separate `--root` graphroot**) serving
the on-disk copy, plus a digest IDMS → `localhost:5000` set to **`mirrorSourcePolicy:
NeverContactSource`** (so a failed local pull never falls back to an unreachable upstream). Running
the registry as a *host* process — not a CRI-O/kubelet workload — keeps its binary and storage outside
the GC/corruption path, which is what breaks the circular dependency above. This is a documented
fallback, not the primary model.

One consequence the build must account for (else the durable copy cannot be read offline):

- **The durable copy must be in CRI-O-readable `containers/storage` format.** An additional image
  store must match the node's storage driver (overlay) and layer format — `create-usb` cannot merely
  drop the OCI layout and register it. The build therefore **populates and validates** a read-only
  c/storage store against the target CRI-O ([§6.4](#64-steps-the-tool-performs) step 4,
  [§6.6](#66-open-tooling-questions)). In the registry fallback the analogous step converts the layout
  into the registry's own storage format instead.

Build-time digest guarantee — scope differs by profile:

- **Primary:** `create-usb` resolves the seed closure + declared extras to digests for p2; there are
  **no p3 manifests on the USB to rewrite**. Digest-pinning of the *site* manifests (and the site
  IDMS) is the site's responsibility, delivered in the data image — the same digest-only discipline
  standard disconnected OpenShift already requires.
- **Secondary:** `create-usb` resolves every reference to a digest **and rewrites the shipped p3
  manifests to digest form** ([§6.4](#64-steps-the-tool-performs) steps 1 and 4), failing the build on
  any reference that cannot be pinned — so no tag path exists at runtime on the forever-offline node.

> **Out of scope — whole-disk failure and durable-copy bit-rot.** No model survives loss of the
> single SNO disk; that is inherent to single-disk hardware and is a re-provision scenario by
> definition, not a design goal here. Likewise, corruption of the *sole durable copy itself* (bit-rot
> of the read-only additional store, or of the registry storage in the fallback) is unrecoverable with
> no network — true of any single on-disk source. The models address the *recoverable* modes — GC
> eviction and corruption of the **writable working copy** (the read-only copy restores it) — not loss
> or corruption of the durable copy.

For the **primary** profile, runtime durability is standard mirror-connected SNO behavior owned by the
site, so the USB feature's obligation is just canonical storage — proven by an install-then-personalize
e2e at a mirror-connected site (mirror reachable, no internet) that force-evicts/corrupts an image and
confirms it recovers from the site mirror. For the **secondary** profile, the read-only additional
image store + `storage.conf` drop-in are net-new (LCA ships no `storage.conf` durability config today)
— this is the highest-risk item and must be proven by an end-to-end test that
force-evicts (and corrupts) an image and confirms recovery from the on-node source (NIC physically
unplugged, no mirror at all).

> **Limitation — runtime-generated tag references are unsupported (secondary profile).** For the
> forever-offline secondary profile, build-time digest-pinning covers everything on the media: the
> seed payload (already digest-referenced) and the shipped p3 manifests/extra-manifests (rewritten to
> digests, [§6.4](#64-steps-the-tool-performs) step 4). It **cannot** cover an image reference *minted
> at runtime* with a **tag** — e.g. a controller/operator that reconciles and creates a `Deployment`
> with a `name:tag` image, or a workload a user applies after install. The shipped mirror is a
> **digest** `ImageDigestMirrorSet` pointing at the local registry; no `ImageTagMirrorSet` is shipped,
> so a runtime tag cannot be resolved even though the matching image is present by digest. Supported
> workloads must therefore reference images by digest; digest-pinned operator catalogs (their CSVs
> already use digests) and the release payload satisfy this, but arbitrary tag-minting operators do
> not. Call this out in user docs. *(The primary profile is a normal mirror-connected SNO at runtime,
> where a stray tag can resolve through the site mirror if the site ships an `ImageTagMirrorSet` — so
> this is not a hard offline constraint there, only the usual disconnected-OpenShift guidance.)*

## 6. USB creation tooling (automated)

Automated, repeatable creation of the USB media is an in-scope deliverable. The tool runs on a
**connected provisioning workstation** (with registry access and pull secret) — never on the
target node — and turns a single input manifest into ready-to-boot USB media.

### 6.1 Prerequisites

The provisioning workstation running `lca-cli create-usb` must have all of the following
reachable (the target node never needs them — only the workstation does, at media-build time):

1. **Seed image created and available** — the SNO seed image referenced by the manifest
   (`seedImage`) has already been generated (see
   [seed-image-generation.md](../seed-image-generation.md)) and pushed to a registry the
   workstation can pull from.
2. **HTTPS server hosting the RHCOS live ISO is reachable** — the tool fetches the base
   RHCOS live ISO (for p1) over **HTTPS** and verifies its published checksum/signature before
   using it as boot media (plain HTTP or unverified downloads are rejected — see
   [§7.5](#75-media-integrity-secure-boot-signing-deferred)).
3. **Container image registry is reachable** — the registry (or mirror) holding the seed's
   referenced images (release payload, operators, recert, lca-cli) is reachable so the tool can
   mirror them into the p2 OCI layout, authenticated with `--auth-file`.
4. **A valid pull secret is available** — the workstation has a pull secret with credentials for
   the seed and image registries (passed via `--auth-file`) so the tool can pull the seed and
   mirror all referenced images.

### 6.2 Form

Recommended: a new `lca-cli` subcommand, **`lca-cli create-usb`**, alongside the existing
`create` command (`lca-cli/cmd/`). Rationale: it reuses in-tree building blocks — the seed's
`containers.list` logic from `lca-cli/seedcreator/seedcreator.go:282`, the config/ops helpers,
and the `containers/image` + `coreos-installer` toolchain lca-cli already depends on — and it
ships in the same binary operators already run. Alternatives (adjustable): a standalone
`hack/create-usb.sh` that graduates to Go, or a container image wrapping the tool for CI/pipeline
use. Whatever the form, the logic below is the contract.

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

**`mode` (profile selector).** `install-only` (primary) builds **p1 + p2 + p4** and requires only the
prepare/image inputs above; `self-contained` (secondary) builds **p1 + p2 + p3 + p4** and additionally
requires `clusterManifests`/`extraManifests` and `siteConfig`. `create-usb`
**fails the build** if a `self-contained`-only field appears under `install-only` (or vice-versa) — the
mode makes each profile's inputs explicit rather than inferred. In `install-only` mode the manifest is
just `IBIPrepareConfig` (with the new `LocalImagesPath`/`Disconnected` fields from
[§8.A](#8-work-items)) plus optional `extraPrecacheImages`; in `self-contained` mode it also unions in
`SeedReconfiguration` (`api/seedreconfig/seedreconfig.go`).

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

**Install-only carries no site config.** All personalization inputs (`siteConfig`, `clusterManifests`,
the runtime pull secret, and the mirror/IDMS config) are delivered **later, at the site, in the data
image** via the existing mechanism ([§3.1](#31-config-delivery-by-labeled-block-device-already-works),
[§5.2](#52-local-resolution-and-runtime-durability)) — `create-usb` neither renders p3 nor synthesizes
any mirror artifact for this profile. `extraPrecacheImages` is the *only* content knob, and it is a
warm-start optimization: anything not precached pulls from the site mirror at first boot.

**Runtime durability artifact (secondary only, fixed).** For `self-contained`, `create-usb` always
builds (in [§6.4](#64-steps-the-tool-performs) step 4) a **read-only `containers/storage` store** from
the closure and a `storage.conf` drop-in registering it under `additionalimagestores` — there is no
selector, this is the single durability model, and it ships **no registry and no IDMS**
([§5.2](#52-local-resolution-and-runtime-durability)). Should the additional-store compatibility spike
fail ([§6.6](#66-open-tooling-questions)), the documented fallback instead synthesizes a digest IDMS
(canonical → `localhost:5000`) plus a host-level registry. (The primary profile has no such artifact —
its site mirror/IDMS comes from the site data image, not the USB.)

**`rhcosLiveIso` (p1 source).** `create-usb` must select and validate p1 deterministically, so the
ISO source is an explicit field (mapping to the existing `ImageBasedInstallConfig.RHCOSLiveISO`),
not implicit. It carries the fetch `url` (HTTPS only) and a **required** `sha256` the tool verifies
before use ([§6.1](#61-prerequisites)); a mismatch or missing checksum fails the build. A
`--rhcos-live-iso`/`--rhcos-live-iso-sha256` **CLI flag overrides the manifest value** when both are
present (flag wins), so a pipeline can pin a locally-staged ISO without editing the manifest.

**p3 manifest inputs (`clusterManifests` / `extraManifests`) — secondary only.** These name the source
paths (or inline documents) that `create-usb` copies into `cluster-configuration/manifests/` and
`extra-manifests/` respectively — the same files steps 1 and 4 scan for the image closure and rewrite
to digests. Contract: each entry is a file or directory; directories are expanded **non-recursively in
lexical (sorted-filename) order** for deterministic output; entries are copied verbatim (no merge) with
later entries **failing the build on a destination-name collision** rather than silently overwriting.
Without this contract the promised full closure ([§5.2](#52-local-resolution-and-runtime-durability))
cannot be guaranteed. *(In `install-only` mode these fields are absent — the equivalent manifests ride
in the site data image.)*

**Pull secrets — which apply depends on the profile:**

- The **workstation mirror credentials** used to pull the seed and mirror images into p2 are a
  **CLI flag** (`--auth-file`), *not* a manifest field, and are never written to the USB. This applies
  in **both** profiles (the build always mirrors from a connected workstation).
- The **cluster runtime pull secret** applies to **both profiles but is delivered differently**: for
  `self-contained` it is `siteConfig.pullSecret`, baked into p3; for `install-only` it arrives in the
  **site data image** at personalization time, not on the USB. Either way a valid pull secret is
  required on the installed node even fully offline (MCO requires one).

For the `self-contained` profile, `siteConfig.pullSecret` accepts either **literal JSON content** or,
when the value begins with `@`, a **file reference** (e.g. `@/run/secrets/cluster-pull-secret.json`)
that `create-usb` resolves **on the workstation at build time** — it reads the file and bakes the
literal contents into p3's `manifest.json` (`SeedReconfiguration.PullSecret` is written verbatim to the
pull-secret file during reconfigure, so no `@`-resolution happens on the node). `create-usb` **must
fail** if an `@`-referenced file is missing or does not parse as a valid pull secret. The `@` prefix is
a `create-usb` input convenience only; it never appears in the rendered p3 manifest.

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
   **Package the `lca-cli` executable into p1.** The stock RHCOS live ISO does not ship `lca-cli`
   — today it is built only into the operator/runtime container image at
   `/usr/local/bin/lca-cli`, which on this media lives inside the p2 OCI layout. If the ignition
   unit invokes the host command `lca-cli ibi` with no binary on the live filesystem it fails
   `command not found` before any disk preparation runs. So `create-usb` must make the executable
   available to the unit, by **one** of:
   - **Embed the static `lca-cli` binary** into the live ISO via ignition (a file, e.g.
     `/usr/local/bin/lca-cli`, marked executable) — deterministic, no image load, preferred; or
   - **`podman run` the lca-cli image** loaded from the mounted p2 OCI layout with
     `podman load`/`skopeo copy oci:… containers-storage:…` (no registry), then exec `ibi` inside
     it — reuses the shipped image but adds a load step and a container-vs-host path surface.

   v1 uses the embedded-binary path; the embedded unit is tested end-to-end with the exact binary
   that ships, so the "prepare starts at all" path is proven, not assumed ([§8](#8-work-items)).
4. **Build the p3 config tree — `self-contained` only.** *(Skipped entirely for `install-only`, which
   ships no p3; its personalization comes from the site data image.)* Render the `/opt/openshift/...`
   layout from `siteConfig`: `cluster-configuration/manifest.json` (`SeedReconfiguration`),
   `network-configuration/` nmconnection files, `extra-manifests/`. **Rewrite every image reference in
   the rendered manifests to its resolved digest.** Resolving+mirroring an image by digest (step 1) is
   not enough on its own: if a shipped manifest still contains a **tag**, the by-digest image is present
   in `/var/lib/containers` yet CRI-O cannot satisfy the tag reference — the durable copy is stored
   **by digest** and nothing resolves a tag → digest offline ([§5.2](#52-local-resolution-and-runtime-durability)).
   So `create-usb` rewrites each `image:` in `manifests/`/`extra-manifests/` (and any image field in
   `SeedReconfiguration`) from `name:tag` to `name@sha256:...`. **A reference that cannot be expressed
   as a digest is unsupported** on the forever-offline node, and `create-usb` fails the build rather
   than shipping media that pulls at runtime. **Emit the runtime durability artifact**
   ([§5.2](#52-local-resolution-and-runtime-durability)) into the installed stateroot: build a
   **read-only `containers/storage` store** from the closure
   (`skopeo copy oci:<layout> containers-storage:[overlay@<store>]...`), validate it against the target
   CRI-O, and render a **`storage.conf` drop-in** registering it under `additionalimagestores`. This is
   a host-level filesystem artifact, not a cluster manifest — it needs no IDMS and no `post-pivot`
   apply, and it has no registry to bootstrap.

   Should the additional-store compatibility spike fail ([§6.6](#66-open-tooling-questions)), the
   documented fallback instead emits a digest **`ImageDigestMirrorSet`** (canonical source registries →
   `localhost:5000`, `mirrorSourcePolicy: NeverContactSource`) into `cluster-configuration/manifests/`
   plus a host-level registry service — a **digest** IDMS only, never an `ImageTagMirrorSet`. In that
   fallback, note `post-pivot` runs `deleteAllOldMirrorResources` before applying
   `cluster-configuration/manifests/`, so the IDMS must be part of that applied set (it is
   (re)created after the delete), not a pre-existing CR.
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

   Each entry carries the **canonical image name** (the name a pod references and the name the image
   is imported under, [§5.2](#52-local-resolution-and-runtime-durability)) and its
   **digest**, which is authoritative. The **OCI-layout locator is expressed as fields**, not a
   packed string: a top-level `ociLayoutPath` (**relative to the p2 mount root**, e.g. `oci` — never
   prefixed with the `ibi-images` label, which is already the mount) plus a per-image `ociTag`
   (the layout's `org.opencontainers.image.ref.name`, if the mirror tool set one). Import selects the
   image by matching `digest` in the layout's `index.json` at `ociLayoutPath`. The locator is kept as
   fields rather than a packed `oci:…@sha256:…` string because the `containers/image` OCI transport is
   `oci:<path>[:reference]` and does **not** accept `@sha256:` to select a digest — a packed string
   would be an invalid transport reference. The build, precache, and self-check all require this
   **exact set** to match — same count, same digests — and any divergence fails the phase. The embedded path is recorded in the prepare config
   (`ExpectedDigestsPath`, [§8.A](#8-work-items)); without an on-media copy the self-check and
   precache cannot run on the disconnected node, and the list is a superset of the seed
   `containers.list` ([§5.1](#51-image-import-during-prepare)). (Optional,
   requirement-driven: when media signing is enabled — `--sign-key`,
   [§7.5](#75-media-integrity-secure-boot-signing-deferred) — also emit a signed `media.manifest` of
   the immutable content-partition digests (p1–p3, or p1–p2 for `install-only`); off by default.)

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
  the primary model registers a read-only `containers/storage` store via `additionalimagestores`, so
  `create-usb` must build that store (`skopeo copy oci:… containers-storage:[overlay@<store>]…`) in a
  driver/layer format the target RHCOS CRI-O reads, and it must be confirmed that GC and reboot-time
  corruption cleanup leave read-only additional stores untouched on the target CRI-O version. **If the
  spike fails, fall back to a host-level (non-CRI-O) registry** — open sub-questions then are which
  registry to run (stock `docker/distribution` vs. one that serves an OCI layout natively), how the
  layout is converted into its storage, and how the host service is packaged (static binary vs. podman
  `--root`; systemd unit + image, net-new on-node artifacts). Only `install-only` avoids this — its
  runtime durability is the site's external mirror.
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
  `Run()` verifies that the imported image count/digests match the **on-media** digest manifest
  (embedded at `ExpectedDigestsPath`, [§6.4](#64-steps-the-tool-performs) step 6, scoped to the
  profile) and **returns an error on any mismatch or missing image**, so the phase fails *before* the
  terminal action (shutdown/reboot/halt) rather than shipping an incomplete node. The
  `<output>.digests.json` written on the workstation is not present on the disconnected node, so the
  check reads the embedded copy. Fully determinable locally.
- **Reconfigure** (`lca-cli post-pivot`, **`self-contained` only** — in `install-only` this step runs
  at the site from the existing IBI data image and is reported by that mechanism, not the USB): a
  **single** success criterion — `PostPivotConfiguration()` returns nil **and** the node reports
  `Ready` **and** all cluster operators report `Available=True`. (Node-`Ready` alone can be reached
  before operators settle; requiring operators `Available` is the meaningful "cluster is up" gate, so
  it is the one used.) This same criterion is used **both** for the p4 result record and for the
  completion signal — no second, looser definition anywhere.
  - **Clock/RTC precondition (gate, not just a risk).** recert regenerates cluster certificates at
    reconfigure time; with no NTP offline and a warehouse→ship→field time gap, an implausible RTC
    yields bad certificate validity windows and can block `Available=True`
    ([§10](#10-open-questions--risks)). So before recert runs, reconfigure **verifies the system clock
    is plausible** (RTC present and within a sane bound of `seedVersion`'s build/expiry, or a
    `ChronyConfig`-supplied local time source is set); if it is not, the phase **fails to the
    powered-on halt** with a clear `reconfigure.json` reason rather than minting certificates with a
    bad validity window ([§8.F](#8-work-items)).
  - **Owner of the wait + p4 write.** Operators reaching `Available` can take many minutes, well
    after `PostPivotConfiguration()` returns and boot continues, so the wait **must not** block
    inside `post-pivot`. Instead a **dedicated result-writer systemd unit** (a candidate host is the
    existing `lca-cli init-monitor` command, though today it does IBU auto-rollback monitoring, so
    this would be a new mode there) polls the criterion with a timeout and writes
    `reconfigure.json` to p4 — `success` when the criterion is met, `failure` (with the unmet
    condition) on timeout. `post-pivot` itself only records an early `failure` if
    `PostPivotConfiguration()` returns an error; the success record is always the result-writer's
    job. Post-pivot already waits for the kube API (`postpivot.go:191-207`); the new unit adds the
    node-`Ready` + cluster-operator-`Available` polling ([§8.H](#8-work-items)).
    - **Activation + failure propagation.** `After=post-pivot.service` only *orders* the unit; it
      does not start it. The result-writer is **pulled into the boot target** (`WantedBy=multi-user.target`,
      or `Wants=` from the post-pivot unit) so it actually runs, and it declares **`Requisite=post-pivot.service`**
      in addition to `After=`. `Requisite` starts the result-writer **only if `post-pivot` succeeded**:
      if post-pivot failed, the result-writer does not start at all, so its timeout/`success` logic
      can never **overwrite the early `failure` record** post-pivot already wrote. The two records
      are thus mutually exclusive — post-pivot owns the failure path, the result-writer owns the
      success/timeout path — and both outcomes are tested ([§8.H](#8-work-items)).
- **`install-only` caveat:** an `install-only` success certifies *prepare only*, not a working
  cluster — the cluster is validated later, when the site personalizes the node via the existing IBI
  data image. A green prepare result ≠ guaranteed-working node.

### 7.2 Result write-back (primary signal)

Each on-USB phase writes a **per-phase record** (`prepare.json`, and in `self-contained` also
`reconfigure.json` — see the contract in
[§7.4](#74-the-ibi-status-partition-contract-discovery--persistence)) to the writable `ibi-status`
partition (p4, [§4](#4-usb-media-layout)) on **both** the success and failure paths.
`ibipreparation.Run()` writes `prepare.json` before its terminal action (power-off in `install-only`,
reboot in `self-contained`); **this is the only record `install-only` produces on the USB** — its
personalization result is reported by the site data-image flow. If the `self-contained` pivot reboot
fails to transition to the installed disk, the live-ISO unit amends `prepare.json` with a
`pivot: failed` sub-status and halts powered-on rather than idling ([§8.I](#8-work-items)) — so a
`self-contained` stick showing `prepare: success` with **no** `reconfigure.json` and a `pivot: failed`
marker is unambiguously a pivot failure, not a stalled reconfigure. In `self-contained`, the result-writer
unit ([§7.1](#71-per-phase-success-criteria)) additionally writes `reconfigure.json` once the success
criterion is met or on timeout (post-pivot writes an early `failure` record if it errors out first).
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
  installed disk with the USB still inserted
  ([§4.1](#41-first-boot--config-discovery-per-profile)); `install-only` has no on-USB
  reconfigure, so p4 carries only `prepare.json`.
- **Missing / read-only (best-effort, not a precondition).** p4 is **observability, not a required
  precondition** — its absence never changes the terminal action. If p4 is absent or cannot be
  mounted read-write, the phase logs loudly and proceeds to the terminal action **its actual result
  dictates** via the [§7.3](#73-at-a-glance-signal-secondary-best-effort) power-state convention: a
  *successful* phase still powers off (`install-only`) / stays up (`self-contained`); only a *failed*
  phase halts powered-on. p4 unavailability must **not** map to `fail → halt` — doing so would halt an otherwise
  successful install and corrupt the very success signal §7.3 provides. The lost detail is the
  per-phase record only; the coarse power-state signal still reflects the real outcome. A missing p4
  should be flagged by `create-usb` at build time so it is caught before the field.
- **Per-phase records, not one overwrite.** A single `status.json` cannot distinguish "prepare
  done" from "reconfigure not yet started." Write **separate per-phase records** —
  `prepare.json` and `reconfigure.json` (or a keyed object per phase) — each with its own
  result/timestamp, so the stick unambiguously shows how far the install progressed.
- **Atomic & durable.** Write to a temp file, `fsync` it, `rename` into place, then `fsync` the
  directory and `sync` the filesystem **before** `shutdown now` / reboot / halt. Otherwise the
  result can be lost in the buffer cache when the box powers off — defeating the primary signal.

### 7.5 Media integrity (Secure Boot; signing deferred)

The trust boundary for this feature is **physical possession of the stick** (a technician
hand-carries it; [§1](#1-goal)). Given that boundary, v1 leans on the integrity guarantees that
already exist or are cheap, and treats cryptographic media signing as requirement-driven hardening
rather than designed-in default. The three that carry their weight in v1:

- **UEFI Secure Boot is the primary integrity anchor** for the code that executes. Firmware
  validates the p1 boot chain (shim → GRUB → kernel/initramfs) against keys already provisioned in
  the platform (the RHCOS live ISO ships Red Hat-signed shim/GRUB). This is verified independently
  of the media, so it is the real root of trust — not anything the USB asserts about itself. The
  Phase 0 spike ([§9](#9-phasing)) must confirm the assembled stick boots with Secure Boot enabled.
- **Verified ISO source.** `create-usb` fetches the RHCOS live ISO over **HTTPS** and verifies its
  published checksum/signature before it becomes boot media (never plain HTTP, never unverified).
  See [§6.1](#61-prerequisites). Cheap and standard, so it stays in the default flow.
- **Mandatory image self-check (completeness, not crypto).** Prepare fails unless every image in
  the on-media digest manifest is resident locally ([§7.1](#71-per-phase-success-criteria)). This
  is a *correctness* gate — it protects the make-or-break offline-resolution requirement — and is
  independent of any signing decision.

**Deferred — requirement-driven media signing (defense-in-depth).** Signing the content partitions
(p2/p3, which Secure Boot does not cover) defends only a narrow case the physical-possession model
mostly already excludes — an attacker who tampers with the images/config *in transit* but cannot
simply swap the whole stick. It is therefore **not designed in by default**; it is added only when
a customer's supply-chain-integrity mandate requires it (disconnected telco/gov deployments often
do). Deferring it also avoids specifying an elaborate verification scheme on top of an unsolved
primitive — **offline key distribution to the node is the real unknown** ([§6.6](#66-open-tooling-questions)),
and it should not compete for design attention with §5.2 (offline resolution) and the Phase 0
bootability spike, which actually gate the feature. When it is pursued, the shape is: sign a
`media.manifest` of the **immutable** partition digests (p1–p2, plus p3 in `self-contained`; never
the writable p4, whose runtime writes would invalidate a spanning signature), place the detached
signature at a defined
read-only path (e.g. `/ibi/media.manifest.sig` on p1), and have prepare/reconfigure verify it —
with the trust-anchor key delivered via the Secure Boot-validated ignition — before consuming
p2/p3. This is captured as work item [§8.J](#8-work-items), gated on the requirement.

## 8. Work items

### A. Config API — `api/ibiconfig/ibiconfig.go`

- **`mode` is a `create-usb` manifest field, not an `IBIPrepareConfig` field.** The profile selector
  (`install-only` / `self-contained`) gates which manifest inputs `create-usb` requires and what it
  assembles ([§6.3](#63-inputs-single-manifest)); it does **not** need to persist into the on-node
  prepare config, because on-node prepare's behavior is fully determined by `Shutdown` (terminal
  action — power off vs pivot reboot, §6.4 step 3 / [§8.I](#8-work-items)) and `ExpectedDigestsPath`
  (precache scope + self-check, below). Do not add a redundant `Mode` to `IBIPrepareConfig`.
- Add a local-images selector to `IBIPrepareConfig`, e.g. `LocalImagesPath string` (mount
  point / OCI layout on USB), `Disconnected bool`, and `ExpectedDigestsPath string` (on-media
  path to the embedded precache-scope digest manifest — used both as the prepare **precache list**
  ([§8.C](#8-work-items)) and the **self-check** reference ([§7.1](#71-per-phase-success-criteria))).
  The manifest's scope is profile-dependent: **seed closure + optional `ExtraPrecacheImages`** for
  `install-only`, the **full site closure** for `self-contained` — so `ExpectedDigestsPath` alone
  tells on-node prepare what to precache/verify, with no on-node `mode` needed.
- **`install-only` (primary)** carries only prepare/image inputs plus an optional
  `ExtraPrecacheImages []string`; it ships **no** runtime-durability artifact and **no** site
  config — personalization and runtime durability are the site's, delivered later via the existing
  IBI data-image mechanism ([§5.2](#52-local-resolution-and-runtime-durability)).
- **`self-contained` (secondary)** ships a **fixed** on-node durability artifact — a read-only
  `containers/storage` additional image store on the installed disk + a `storage.conf` drop-in (no
  registry, no IDMS) — rendered by `create-usb` ([§6.4](#64-steps-the-tool-performs) step 4). There is
  no selector field: it is the single durability model ([§5.2](#52-local-resolution-and-runtime-durability)),
  and no `siteMirror` option exists — an external mirror is the site's own standard disconnected config
  in the `install-only` flow, never an artifact the USB synthesizes.
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
  ([§5.2](#52-local-resolution-and-runtime-durability), §8.D)** — not the import path, and only the
  `self-contained` (secondary) profile ships an on-node artifact for it. The `install-only` (primary)
  node's runtime durability is the **site's** responsibility once personalized — its standard
  disconnected config (external mirror + IDMS) arrives with the site data image, not from the USB. In
  `self-contained`, a read-only additional image store on the installed disk (a second canonical copy
  of the closure, built from the OCI layout, not served off USB) is CRI-O's durable recovery source;
  this durable copy is distinct from the prepare-time import above.

### C. Wire into prepare — `lca-cli/ibi-preparation/ibipreparation.go`

- Redirect the two pull sites to a **local import by digest** from the mounted layout instead of
  `podman pull`: the seed pull (line 60) and `precacheFlow` (line 73, via `podmanImgPull` /
  `workload.Precache`). Mount USB images first; after precache, unmount the USB. Do **not** set
  `ReleaseRegistry`/`ReplaceImageRegistry` — that rewrites references to non-canonical names
  ([§5.1](#51-image-import-during-prepare)); the import writes canonical refs directly. Only
  `self-contained` additionally persists the on-disk layout copy + runtime registry (§8.B/§8.D).
- **Drive precache from the profile's declared scope, not just the seed `containers.list`.**
  `precacheFlow` currently reads only `common.ContainersListFilePath`; extend it to precache **every
  image in the on-media precache manifest** ([§8.A](#8-work-items)) — for `install-only`, the seed
  closure **+ `ExtraPrecacheImages`** (site-only images are left to the site mirror at
  personalization); for `self-contained`, the **full site closure** emitted by `create-usb` alongside
  p2 (superset of the seed list, including p3-manifest/extra-manifest images). Otherwise, in
  `self-contained`, those images stay on p2, never reach `/var/lib/containers`, and the prepare
  self-check ([§7.1](#71-per-phase-success-criteria)) fails.

### D. Persist the runtime durability artifact into the installed stateroot — `self-contained` only

- The normal path is local: images imported under canonical names by digest are found by CRI-O with
  no mirror. But a single cached copy is not durable — kubelet GC can evict it under disk pressure,
  and CRI-O *deletes* a corrupt image on reboot — so a runtime recovery source is needed. **Only the
  `self-contained` (secondary) profile ships one**, because it is the only profile that personalizes
  offline from the USB ([§5.2](#52-local-resolution-and-runtime-durability)).
- **`install-only` (primary): no artifact from the USB.** The USB only installs + precaches, then
  powers off; runtime durability arrives later with the **site's own** standard disconnected config
  (external mirror + digest IDMS) via the existing IBI data-image mechanism. `create-usb` synthesizes
  **no** mirror artifact for this profile, and the installed stateroot carries none until the site
  personalizes it — this work item does not run for `install-only`.
- **`self-contained` (secondary): on-node source — read-only additional image store.** Persist a
  second, canonical copy of the closure as a **read-only `containers/storage` store** on the installed
  disk (outside CRI-O's writable graphroot) and register it via `additionalimagestores` in
  `storage.conf`. CRI-O resolves closure images from it with **no registry and no IDMS**; the copy
  survives GC eviction and writable-store corruption cleanup because both act only on the writable
  graphroot ([§5.2](#52-local-resolution-and-runtime-durability)). This is the single durability model
  — no on-node registry, no `localhost:5000`, no IDMS in the primary model. Concretely this requires:
  - **The durable store in CRI-O-readable `containers/storage` format** — `create-usb` populates it
    with `skopeo copy oci:<layout> containers-storage:[overlay@<store>]...` and **validates** it
    against the target CRI-O ([§6.4](#64-steps-the-tool-performs) step 4); an additional store must
    match the node's storage driver/layer format, so this is a compat gate ([§6.6](#66-open-tooling-questions)).
  - **A `storage.conf` drop-in** registering the read-only store under `additionalimagestores`,
    rendered into the installed stateroot by `create-usb` ([§8.E](#8-work-items)). No systemd service,
    no registry image in the closure — nothing to bootstrap.
- **Registry fallback only (if the compat spike fails).** Persist a host-level (non-CRI-O) registry
  service serving the on-disk copy + a **digest** IDMS → `localhost:5000` with `mirrorSourcePolicy:
  NeverContactSource` ([§5.2](#52-local-resolution-and-runtime-durability)). Only in this fallback do
  the registry's own image (in the closure), a systemd unit, and layout→registry-storage conversion
  apply — run as a *host* process (static binary in the stateroot, or podman with a separate `--root`)
  so its image/storage sit outside the GC/corruption path. The IDMS must land in the
  `cluster-configuration/manifests/` set that `post-pivot` applies, because `post-pivot` runs
  `deleteAllOldMirrorResources` first ([§6.4](#64-steps-the-tool-performs) step 4). The primary
  additional-store model ships no IDMS.

### E. USB media creation tooling (automated) — see [§6](#6-usb-creation-tooling-automated)

- Implement `lca-cli create-usb` (per [§6](#6-usb-creation-tooling-automated)): manifest-driven,
  produces a `.img` (or writes a block device) with p1 live ISO + embedded ignition, p2 `ibi-images`
  OCI layout, an empty writable p4 `ibi-status` (result partition — see item H below), and — **only
  for `self-contained`** — a p3 `cluster-config` tree (`install-only` media omit p3 entirely;
  personalization comes from the site data image, [§4](#4-usb-media-layout)).
- **Package the `lca-cli` executable into p1** ([§6.4](#64-steps-the-tool-performs) step 3): the
  stock RHCOS live ISO has no `lca-cli` (it ships only inside the p2 runtime image), so the embedded
  ignition unit's `lca-cli ibi` would fail `command not found`. Embed the static binary via ignition
  (v1) and **test the embedded unit end-to-end with the exact binary that ships**.
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
- **Emit the runtime durability artifact — `self-contained` only** ([§6.4](#64-steps-the-tool-performs)
  step 4, [§8.D](#8-work-items)). Primary model: populate a **read-only `containers/storage` store**
  (`skopeo copy oci:<layout> containers-storage:[overlay@<store>]...`), validate it against the target
  CRI-O, and render a **`storage.conf` drop-in** registering it under `additionalimagestores` into the
  installed stateroot — **no IDMS, no registry**. Registry fallback only: a digest IDMS →
  `localhost:5000` (`mirrorSourcePolicy: NeverContactSource`, digest only, no `ImageTagMirrorSet`) plus
  a **host-level** (non-CRI-O) registry service — a systemd unit and registry storage converted from
  the OCI layout, run as a static binary or podman with a separate `--root` so it sits outside the
  GC/corruption path. `install-only` emits neither; the site supplies its own mirror config.
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
  discover by FS label, per-phase records (`prepare.json`, plus `reconfigure.json` in
  `self-contained`), atomic write + `fsync`/`sync` before power-off/reboot/halt, and graceful degrade
  when p4 is missing/read-only.
- Write the `prepare.json` record + log bundle from `ibipreparation.Run()` (both success and
  failure paths, before the terminal action). **This is the only p4 record `install-only` produces**
  — its personalization success is reported later by the existing site data-image flow, not on the
  USB.
- **`self-contained` only** — add a **dedicated result-writer systemd unit** (candidate host:
  `lca-cli init-monitor`, today an IBU auto-rollback monitor — this is a new mode) that polls the
  reconfigure success criterion with a timeout and writes `reconfigure.json` to p4 — so the long
  operators-`Available` wait does not block `post-pivot` ([§7.1](#71-per-phase-success-criteria)).
  The unit is **activated** by the boot target (`WantedBy=multi-user.target`/`Wants=`) — `After=`
  alone would not start it — and declares **`Requisite=post-pivot.service`** so it runs *only if
  post-pivot succeeded*; if post-pivot errors out it writes an early `failure` record and the
  result-writer never starts, so a timeout result can never overwrite that failure. Test both
  outcomes.
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

## 10. Open questions / risks

- **Digest vs. tag resolution offline ([§5.2](#52-local-resolution-and-runtime-durability))** — the make-or-break correctness point; needs a
  disconnected e2e test confirming every image resolves locally under its canonical name by digest
  (normal path needs no mirror) and that no stray runtime **tag** reference exists — the shipped
  mirror is a digest IDMS, so a tag reference has no offline resolution.
- **Runtime image durability & recovery ([§5.2](#52-local-resolution-and-runtime-durability))** — a single cached copy is not
  durable: kubelet GC evicts under disk pressure and CRI-O deletes a corrupt image on reboot. The
  recovery source is profile-conditional: **`install-only` (primary)** relies on the **site's own**
  standard disconnected config (external mirror + digest IDMS) delivered with the data image — the
  USB ships no durability artifact; **`self-contained` (secondary)** carries its own source
  (a read-only additional image store on disk that survives GC and reboot corruption cleanup; no
  registry, no IDMS). Each profile needs an
  e2e test that evicts/corrupts an image
  and confirms recovery (`install-only`: site mirror reachable, no internet; `self-contained`: NIC
  unplugged, no mirror). Whole-disk failure is out of scope (re-provision).
- **Site personalization contract (`install-only`)** — the USB deliberately delegates personalization
  and runtime durability to the site's existing IBI data-image flow. Confirm the site's standard
  disconnected config (external mirror + IDMS, pull secret, site manifests) holds the full closure the
  installed node will reference and is delivered via virtual media as it is for non-USB IBI today; the
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
