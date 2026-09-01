# Design: LUKS root-filesystem encryption (TPMv2) during Image-Based Installation

**Status:** Proposal
**Scope:** Image-Based Installation (IBI) for Single Node OpenShift (SNO)
**Author:** (draft)
**Date:** 2026-09-01

## 1. Summary

Enable LUKS2 encryption of the **root filesystem**, bound to the platform **TPMv2** via
Clevis, as part of the IBI install flow — so the node comes up encrypted with no post-deploy
step and no operator interaction at boot. Target: physically exposed far-edge SNO where a
stolen disk must not reveal cluster data.

The encryption **engine** lives in lifecycle-agent's `lca-cli` (the component that actually
writes the disk). It generates an Ignition config declaring a TPM2-bound LUKS root device and
passes it to `coreos-installer install --ignition-file`; on first boot RHCOS reprovisions the
root partition into LUKS2 and Clevis seals the key to the TPM2. No custom bootloader/dracut/
crypttab code.

**This is a two-repo feature** (see [Section 3](#3-verified-end-to-end-ibi-architecture-why-this-is-a-two-repo-change-not-an-ibi-operator-change), verified against the code):
- **lifecycle-agent** — three touch points: (a) the install-time encryption engine
  (`lca-cli ibi`) + the `IBIPrepareConfig` field; (b) the **IBU Prep path** and (c) the
  **`IPConfig` CRD pivot path** — both stateroot-deploy paths must propagate the node's existing
  unlock config into the new stateroot so encryption survives a pivot ([Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api)).
- **openshift/installer** — the user-facing `ImageBasedInstallationConfig` knob and the
  `ibi-configuration.json` contract that carries it to `lca-cli`. Unchanged by the IBU work.
- **image-based-install-operator (IBI Operator) — NOT involved** (it runs only the post-write
  config phase; [Section 3](#3-verified-end-to-end-ibi-architecture-why-this-is-a-two-repo-change-not-an-ibi-operator-change)).

### Goals
- **Encrypt the root filesystem at install time.** The node's root FS — etcd, secrets, `/etc`,
  kubelet state, and logs — is written as a TPM2-bound LUKS2 container during the IBI flow, so it
  is protected from the moment the machine first boots. There is no window in which a fully
  provisioned node sits on disk unencrypted, and no day-2 re-provisioning step for an operator to
  run or forget. This directly answers the target threat: a **disk (or whole node) physically
  removed from a far-edge site must not yield cluster data**.
- **Unlock unattended, with no operator interaction.** The key is sealed to the platform's TPM2
  via Clevis, so the node decrypts and boots on its own after any power-cycle or reboot — no
  passphrase prompt, no remote key server, no human at the console. This is mandatory for the
  far-edge deployment model, where nodes are lights-out and may reboot with nobody on site
  ([Section 7](#7-tpm2-binding-policy)).
- **Survive an Image-Based Upgrade (IBU) pivot — hard requirement.** After an upgrade the new
  stateroot must continue to auto-unlock the root FS via TPM2, unattended, with no re-enrollment
  and no manual step. This is treated as a shipping requirement, not a follow-up: an encrypted
  node that cannot be upgraded (or that silently loses auto-unlock on upgrade) is not viable in
  the field. Rollback to the previous deployment must likewise keep auto-unlocking
  ([Section 8](#8-ibu-upgrade-interaction--required)).
- **Reuse the supported RHCOS/Ignition root-reprovisioning path; minimal custom code.** Encryption
  is expressed as native Ignition that RHCOS executes on first boot — no custom bootloader,
  dracut, crypttab, or key-management code for LCA to own and maintain. Staying on the supported
  upstream path keeps the feature small and its long-term maintenance burden low
  ([Section 2](#2-background-how-rhcos-tpm2-root-luks-works), [Section 5](#5-ignition-generation-lifecycle-agent)).
- **Opt-in, with default behavior byte-identical to today.** Encryption is enabled only by an
  explicit config field; when it is unset, the install and upgrade paths behave exactly as they do
  now. No existing deployment is affected, and the feature can be adopted per-cluster.
- **No new user-facing API for the upgrade path.** Encryption is auto-detected as a node-intrinsic
  property during upgrade Prep, so the `ImageBasedUpgrade` CR stays encryption-agnostic — an
  encrypted node "just upgrades" with no extra fields to set
  ([Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api)).

### Non-goals (this iteration)
- Encrypting the `/var/lib/containers` extra partition (precached images stay plaintext; [Section 9](#9-deferred-varlibcontainers-partition-residual-risk)).
- Tang / NBDE. **Out of scope permanently** — there is no requirement to ever support Tang.
  TPM2 is the only mechanism.
- **Firmware TPMs (Intel PTT, AMD fTPM) and virtual TPMs are not a supported configuration.**
  Only a **discrete hardware TPM2** is supported; see [Section 7](#7-tpm2-binding-policy) for the rationale.
- Re-encryption / key rotation *on* an IBU pivot. The existing LUKS header + TPM2 binding are
  reused across the pivot; only the *boot-unlock config* is propagated ([Section 8](#8-ibu-upgrade-interaction--required)). Surviving the pivot
  is required; re-keying during it is not.
- **Disabling encryption after install.** Once a node is installed encrypted, there is **no
  supported path to turn LUKS off in place** — not day-2, and not via an IBU pivot (which
  deliberately *re-applies* the unlock config, [Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api)). See [Section 7](#7-tpm2-binding-policy) for why, and the
  supported alternative (reprovision). Encryption is a one-way, install-time decision in this
  design.
- Seed-image *encryption* — the seed is **never encrypted** (permanent constraint). Encryption
  is strictly a target-side property, applied at install and propagated across pivots ([Section 8](#8-ibu-upgrade-interaction--required)). A
  seed captures filesystem content, not the block-level LUKS, so it carries no unlock config —
  which is precisely why LCA must re-apply `rd.luks.*` on every pivot ([Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api)). The one seed
  *requirement*: its RHCOS initramfs must ship the clevis/tpm2 dracut modules ([Section 8.4](#84-seed-constraint-not-seed-encryption)).

## 2. Background: how RHCOS TPM2 root LUKS works

- `coreos-installer install` has **no encryption flags**; encryption is declared in an
  Ignition config embedded via `--ignition-file` and executed on **first boot in the
  initramfs** (root reprovisioning).
- Root reprovisioning: Ignition copies root content to RAM (**≥ 4 GiB RAM**, plus enough to
  stage the root FS), wipes the partition (`wipeVolume: true`), creates the LUKS2 container
  (located by `by-partlabel/root`), Clevis-binds TPM2, recreates the FS labeled `root`, copies
  content back. Requires **Ignition ≥ 3.3.0**. `/boot` + ESP stay plaintext (kernel/initramfs).
- TPM2-only (no PCRs) unlocks unattended and survives firmware/kernel updates; PCR binding
  adds tamper detection but breaks on measured-boot changes.

## 3. Verified end-to-end IBI architecture (why this is a two-repo change, not an IBI-Operator change)

IBI is split into two phases with **different tools and different config**:

### Phase 1 — Preinstallation / imaging (the disk is written here)
`openshift-install image-based create image` consumes **`ImageBasedInstallationConfig`**
(v1beta1) and builds the **live installation ISO**. That ISO enables one systemd unit,
`install-rhcos-and-restore-seed.service`, whose script
(`data/data/imagebased/files/usr/local/bin/install-rhcos-and-restore-seed.sh`) pulls `lca-cli`
out of the seed image and runs:

```
/usr/local/bin/lca-cli ibi -f /var/tmp/ibi-configuration.json
```

So **`lca-cli` (lifecycle-agent) is the disk writer** — it runs `coreos-installer`, sets up
the stateroot from the seed, and precaches. openshift/installer never calls `coreos-installer`
for IBI itself.

**The installer→lca-cli contract is a JSON file.** openshift/installer marshals an internal
`ibiConfigurationFile` struct (`pkg/asset/imagebased/image/ignition.go:45-56`, populated at
`:95-106`) into `ibi-configuration.json`, which `lca-cli ibi -f` deserializes into
lifecycle-agent's `IBIPrepareConfig` (`api/ibiconfig/ibiconfig.go:68`; JSON tags match
one-for-one). **This JSON is the only channel from installer config to the target disk.**

Important: `ignitionConfigOverride` is **not** in that contract and is **not** a route for
root encryption — see [Section 11](#11-alternatives-considered).

### Phase 2 — Deployment / config (post-write)
Driven by the **IBI Operator** (`ImageClusterInstall` CR) *or* `openshift-install image-based
create config-image`. Produces only the **configuration ISO** (cluster-specific reconfig via
`SeedReconfiguration`), applied *after* the disk is already written.

Verified (image-based-install-operator repo): it never sets `BMH.Spec.Image`, disables Ironic
disk cleaning, has **zero** `coreos-installer`/disk-write code, and `ImageClusterInstallSpec`
has **no** disk/seed/encryption fields. **The IBI Operator cannot carry install-time
encryption** — it is not in the disk-write path. This is why the knob must live in Phase 1.

```
Phase 1 (writes disk):
  ImageBasedInstallationConfig ──openshift-install──▶ live ISO
        │  (diskEncryption)                              │ runs
        └──────────────────────────────▶ ibi-configuration.json ──▶ lca-cli ibi
                                                                        │
                                                        coreos-installer install --ignition-file
                                                                        │
                                              first boot: RHCOS reprovisions root → LUKS2 + TPM2
Phase 2 (post-write, NOT involved in encryption):
  ImageClusterInstall / config-image ──▶ config ISO ──▶ SeedReconfiguration
```

What lands on which storage (default SNO; `/var` is part of root, `/var/lib/containers` is a
separate partition):
| Data | Location | Encrypted here? |
|------|----------|-----------------|
| etcd, secrets, `/etc`, kubelet, logs | root FS | **Yes** |
| Precached container images | `/var/lib/containers` partition | No ([Section 9](#9-deferred-varlibcontainers-partition-residual-risk)) |
| Kernel / initramfs | `/boot`, ESP | No (standard) |

## 4. User-facing API (two structs, same shape)

The field must exist in **both** structs on the Phase-1 path:

**lifecycle-agent** — `api/ibiconfig/ibiconfig.go`, on `IBIPrepareConfig` (the struct `lca-cli
ibi` reads, used by both the installer-driven and standalone-CLI flows):

```go
type IBIPrepareConfig struct {
    // ... existing fields ...

    // DiskEncryption, when set, enables TPM2-bound LUKS encryption of the root
    // filesystem at install time. Nil = not encrypted (default, unchanged).
    // +optional
    DiskEncryption *DiskEncryption `json:"diskEncryption,omitempty"`
}

// DiskEncryption configures TPM2-bound root-filesystem LUKS encryption.
// TPM2 is the only mechanism (Section 1), so there is no `type` discriminator —
// a non-nil block means TPM2-bound root LUKS.
type DiskEncryption struct {
    // PCRList: optional comma-separated PCR indices (e.g. "1,7"). Empty (default)
    // binds to TPM presence only — no PCR policy — so firmware/kernel updates and
    // future IBU pivots do NOT break auto-unlock. See Section 7.
    // +optional
    PCRList string `json:"pcrList,omitempty"`
}
```

**openshift/installer** — `pkg/types/imagebased/imagebased_config_types.go`, an equivalent
`DiskEncryption` type + `DiskEncryption *DiskEncryption` field on the **`InstallationConfig`**
struct (place it after `CoreosInstallerArgs`), with matching JSON tags so it round-trips
through `ibi-configuration.json`. Note: the field must be **first-class** — `CoreosInstallerArgs`
is allow-listed to `--append-karg`/`--delete-karg`/`--save-partlabel`/`--save-partindex`, so
encryption cannot be smuggled through it. Add it to `InstallationConfig` (the imaging/Phase-1
resource), **not** the sibling `Config` (Phase-2 config-image) struct, which has no disk fields.

For reference, `InstallationConfig` today carries: required `installationDisk`, `pullSecret`,
`seedImage`, `seedVersion`; optional `additionalTrustBundle`, `architecture`,
`extraPartition{Label,Number,Start}`, `ignitionConfigOverride` (Ignition 3.2 only),
`imageDigestSources`, `networkConfig`, `proxy`, `releaseRegistry`, `shutdown`,
`skipDiskCleanup`, `sshKey`, `coreosInstallerArgs`. `diskEncryption` joins this set.

Example `image-based-installation-config.yaml`:
```yaml
apiVersion: v1beta1
kind: ImageBasedInstallationConfig
seedImage: quay.io/example/seed:4.19
seedVersion: "4.19.0"
installationDisk: /dev/sda
diskEncryption: {}      # presence enables unattended TPM2-only root LUKS
```

### Validation
- lifecycle-agent `IBIPrepareConfig.Validate()` (ibiconfig.go:224) and openshift/installer
  `validate()` (`pkg/asset/imagebased/image/imagebased_config.go:165-206`): if
  `DiskEncryption != nil` and `PCRList` set, each element parses as an integer in `[0,23]`.
- No `type`/mechanism validation — TPM2 is implied.

## 5. Ignition generation (lifecycle-agent)

Vendored `github.com/coreos/ignition/v2/config/v3_4/types` provides `Luks`, `Clevis`, `Tpm2`
(`vendor/.../v3_4/types/luks.go`, `clevis.go`). **Butane is not vendored**, so construct the
config from these typed structs directly (consistent with `postpivot.go:836` building Ignition
programmatically).

New file `lca-cli/ibi-preparation/ignition.go`:
```go
// buildRootLuksIgnition returns an Ignition v3.4.0 config that reprovisions the
// existing root partition into a TPM2-bound LUKS2 container.
func buildRootLuksIgnition(enc *ibiconfig.DiskEncryption) (types.Config, error)
```
Emits (JSON):
```json
{
  "ignition": { "version": "3.4.0" },
  "storage": {
    "luks": [{ "name": "root", "device": "/dev/disk/by-partlabel/root",
               "label": "luks-root", "wipeVolume": true, "clevis": { "tpm2": true } }],
    "filesystems": [{ "device": "/dev/mapper/root", "format": "xfs",
                      "label": "root", "wipeFilesystem": true }]
  }
}
```
Rules: `device: by-partlabel/root`; `wipeVolume: true` + filesystem `label: root` are
mandatory for root reprovisioning; never set `path: "/"`; `clevis.tpm2: true` default, or
`clevis.custom` with a `tpm2` pin + PCR config when `PCRList` is set.

Assumption: the root filesystem `format` is **xfs** — RHCOS's default rootfs — so the generated
config hardcodes `"format": "xfs"`. This holds for stock RHCOS seeds; if a seed ever shipped a
non-xfs root the format would have to match it (worth confirming alongside spike S8, [Section 12](#12-test--validation-plan)).

## 6. Wiring into the disk write (lifecycle-agent)

Single integration point in `diskPreparation()` (`lca-cli/ibi-preparation/ibipreparation.go`),
around the `coreos-installer install` call (ibipreparation.go:188):

```go
installArgs := []string{"install", i.config.InstallationDisk}
if i.config.DiskEncryption != nil {
    ignPath, err := i.writeRootLuksIgnition() // build + marshal + write under common.IBIWorkspace
    if err != nil { return fmt.Errorf("failed to prepare disk-encryption ignition: %w", err) }
    installArgs = append(installArgs, "--ignition-file", ignPath)
}
installArgs = append(installArgs, i.config.CoreosInstallerArgs...)
if _, err := i.ops.RunInHostNamespace("coreos-installer", installArgs...); err != nil { ... }
```

Seed-ostree deploy and precache (ibipreparation.go:68,88) are unchanged — they write the
mounted plaintext partitions during install; encryption happens on the target's **first boot**,
staging the deployed seed content through RAM. No `ops` interface change ⇒ **no `make generate`**
for this scope.

## 7. TPM2 binding policy
- **Discrete hardware TPM2 only (supported configuration).** The key is sealed to a **physical,
  discrete TPM2 chip**. **Firmware TPMs** (Intel PTT, AMD fTPM) and **virtual TPMs** are **not a
  supported configuration** for this feature, even though they present the same TPM2 interface —
  their key material lives in firmware/hypervisor state rather than dedicated tamper-resistant
  hardware, which undercuts the stolen-disk threat model ([Section 1](#1-summary)) and makes unlock behavior across
  firmware updates less predictable. A virtual TPM may be used for early functional development,
  but every support decision and final validation must be against a discrete hardware TPM ([Section 12](#12-test--validation-plan)).
  This is a **support-policy statement, not enforced in code** — firmware and virtual TPMs present
  an identical TPM2 interface, so there is no reliable install- or upgrade-time check that
  distinguishes them; the feature binds to whatever TPM2 is present, and running it on an
  unsupported TPM is simply an unsupported configuration.
- **Default: TPM2 presence only (no PCRs)** — survives firmware/kernel updates and the IBU
  pivot ([Section 8](#8-ibu-upgrade-interaction--required), required); right for unattended far-edge. This default is load-bearing for IBU:
  PCR binding would break unlock when the upgrade changes the measured boot chain.
- **Optional `pcrList`** — tamper detection at the cost of re-enrollment after measured-boot
  changes; if used, prefer stable PCRs (e.g. 7) and document recovery.
- **No passphrase** ⇒ losing the TPM = data loss by design. A future recovery-key escrow option
  is out of scope; document the caveat.
- **Encryption cannot be disabled in place.** There is no supported way to remove LUKS from an
  installed node's root once it is encrypted. Encryption is established only by first-boot
  Ignition reprovisioning ([Section 5](#5-ignition-generation-lifecycle-agent)), which runs exactly once; RHCOS is an immutable,
  ostree-based OS with no "un-encrypt" flow, and root unlock is wired into the boot chain via
  `rd.luks.*` kargs and the on-disk clevis token ([Section 8.2](#82-what-actually-unlocks-root--and-what-the-pivot-drops)). While `cryptsetup reencrypt
  --decrypt` can in principle decrypt a LUKS2 container, doing it to a live RHCOS root would
  also require stripping the boot-time unlock config (kargs, device-mapper name, FS label) and
  is not a validated operation — and an IBU pivot re-applies that config rather than removing it
  ([Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api)). The supported way to obtain an unencrypted node is to **reprovision** via IBI with
  `diskEncryption` unset (and restore from backup), not to mutate a running node.

## 8. IBU (upgrade) interaction — REQUIRED

An encrypted node **must** survive an Image-Based Upgrade: after the pivot the new stateroot
must auto-unlock the root FS via TPM2, unattended. Hard requirement, not a follow-up.

### 8.1 Why the container survives a pivot for free
The LUKS2 container sits at the **root-partition** level (`by-partlabel/root`), *below* the
OSTree stateroot layer. An IBU pivot deploys a new stateroot *inside* the already-unlocked root
FS (`ostree admin deploy` — it never reformats the partition). So the LUKS header, the Clevis
TPM2 binding, and the on-disk ciphertext are untouched by a pivot. With the **no-PCR default
([Section 7](#7-tpm2-binding-policy))** the TPM2 unseal does not depend on measured-boot state, so the new
kernel/initramfs/bootloader delivered by the upgrade does not break unlock *cryptographically*.

**Rollback needs no propagation.** An IBU Rollback re-selects the *previous* deployment, whose
bootloader entry already carries its own `rd.luks.*` kargs (it was the running, encrypted node).
Only the *newly deployed* stateroot drops them ([Section 8.2](#82-what-actually-unlocks-root--and-what-the-pivot-drops)), so the karg propagation is required on the
forward pivot only; a rollback boots straight into an entry that already auto-unlocks (verified by
spike S7, [Section 12](#12-test--validation-plan)).

### 8.2 What actually unlocks root — and what the pivot drops
How RHCOS unlocks an encrypted root at boot: root is opened **in the
initramfs**, before `/sysroot` is mounted, from three inputs — (a) the **clevis TPM2 token in
the on-disk LUKS2 header** (survives a pivot; it lives on the partition), (b) the **initramfs
clevis/dracut modules** (RHCOS ships these by default via ignition-dracut — [Section 8.4](#84-seed-constraint-not-seed-encryption)), and (c) the
**kernel arguments** that identify the root LUKS device (`rd.luks.*`). Because root is mounted by
the initramfs, the **`/etc/crypttab` inside the deployed root cannot affect root unlock** — that
file governs *non-root* devices only. So the load-bearing per-deployment artifact for root is
the **kargs**.

The IBU Prep path drops exactly that, because the seed is unencrypted:

1. **kargs (load-bearing)** — `internal/prep/prep.go:173-181` builds the new deployment's kargs
   *fresh* from the booted node's MachineConfig (`utils.BuildKernelArguments*`,
   `utils/utils.go:592-609`; here `BuildKernelArgumentsFromMCOFile`, appended as `--karg` pairs),
   then `ostreeClient.Deploy(...)` runs
   `ostree admin deploy --os <os> --no-prune <kargs...> <refspec>`
   (`internal/ostreeclient/ostreeclient.go:63-91`). No `--karg-none`, so ostree *may* inherit the
   booted deployment's `rd.luks.*` as a base — but that's not guaranteed for a freshly
   `os-init`'d stateroot and **must not be relied on**. The seed's MachineConfig has no
   `rd.luks.*`, so the append step won't re-add them.
2. **/etc (not load-bearing for root)** — `internal/prep/prep.go:206-215` replaces `/etc`
   wholesale from the seed's `etc.tgz` (no 3-way merge), so a running node's `/etc/crypttab` is
   not carried over. Per the above this does **not** by itself break *root* unlock, but it drops
   config for any non-root encrypted mounts — carry it forward for correctness ([Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api)).

(No existing LUKS/crypttab/clevis handling anywhere in the code, and no CRD/seedreconfig surface
for extra kargs or /etc files — verified.)

### 8.3 Design: propagate the running node's unlock config (no new IBU API)
IBU needs **no new CRD/API surface** for encryption. Encryption is an auto-detected,
node-intrinsic property; during Prep, LCA detects the booted root is LUKS-encrypted and
replicates *that node's own* unlock configuration into the new deployment:

- **Detect** — the booted root is a LUKS device. Signals available today: `rd.luks.*` in
  `/proc/cmdline`, or `lsblk -f` reporting `crypto_LUKS` on the root device (LCA already runs
  `lsblk -f` at `ops.go:500`, and `ops.ReadFile`, `ops.go:570`, can read `/proc/cmdline`). LCA
  must read the current `rd.luks.*` regardless, in order to propagate them. If not encrypted,
  behavior is byte-identical to today.
  Both reads must see the **host**, not the container: Prep runs inside the `ibu-stateroot-setup`
  Job, so `lsblk` must go through `ops.RunInHostNamespace` (as `ops.go:500` already does).
  `/proc/cmdline` is the host kernel command line and is not namespaced, so reading it from the
  Job still reflects the booted node.
- **kargs (required — load-bearing for root, [Section 8.2](#82-what-actually-unlocks-root--and-what-the-pivot-drops))** — add the node's existing `rd.luks.*` args
  (as `--karg` pairs) to the `kargs` slice at `internal/prep/prep.go:173-181` *before* `Deploy`,
  so they are explicit and independent of ostree inheritance.
- **/etc/crypttab (secondary — non-root only)** — *not* required for root unlock ([Section 8.2](#82-what-actually-unlocks-root--and-what-the-pivot-drops)). Only
  needed if the node has non-root encrypted mounts; if so, write the node's `/etc/crypttab` into
  the new `deploymentDir/etc` after the `etc.tgz` extraction (`internal/prep/prep.go:~213`), or
  via post-pivot reconfig (`lca-cli/postpivot/postpivot.go`).

This keeps the IBU CR encryption-agnostic and makes an encrypted node "just upgrade."

**Both stateroot-deploy paths need the karg propagation.** Two separate, both-live paths deploy
a fresh stateroot and reboot, and **both build kargs from the same MCO source**
(`utils.BuildKernelArguments*`), so both lose `rd.luks.*`:

- **(A) IBU stage machine** (Idle→Prep→Upgrade) — `prep.SetupStateroot` via the
  `ibu-stateroot-setup` Job (`lca-cli/cmd/ibuStaterootSetup.go:89`, `controllers/prep_handlers.go:584`);
  deploys the **seed** commit.
- **(B) `IPConfig` CRD** (SNO IP reconfiguration, "two stateroots, one reboot";
  `lca-cli/ipconfig/prepivot.go`, `docs/ip-configuration.md`); deploys the **currently-booted
  commit** (`deployNewStateroot`, called at `:262-266`, defined at `:315-341`).

They are mutually gated (`gateIBUByIPConfig` / `gateIPConfigByIBU`) but each independently pivots
an encrypted node, so the **detect + inject-`rd.luks.*`-kargs** helper must run in **both**. The
`/etc/crypttab` handling differs but matters only for *non-root* devices ([Section 8.2](#82-what-actually-unlocks-root--and-what-the-pivot-drops)): path A replaces
`/etc` from the seed (so carry crypttab forward there if non-root mounts exist), while path B
already copies `/etc` forward from the running deployment (`copyEtc`, `:405-418`).

**Note — `prep.SetupStateroot` (path A) is shared with the install flow.** The same function is
*also* called during first-time IBI install (`lca-cli/ibi-preparation/ibipreparation.go:68`), where
encryption is instead established by the first-boot Ignition path ([Section 5](#5-ignition-generation-lifecycle-agent)–[Section 6](#6-wiring-into-the-disk-write-lifecycle-agent)), not by kargs. So the
detect-and-inject-`rd.luks.*` step added here must be a no-op at install time. It naturally is:
during install the booted environment is the live installer ISO, whose root is **not** LUKS, so
`isRootEncrypted()` returns false. Belt-and-suspenders, `SetupStateroot` already receives an
`ibi bool` (`prep.go:94`; `true` for install per `ibipreparation.go:68`, `false` for IBU per
`ibuStaterootSetup.go:89`) that can gate the propagation to the IBU path explicitly.

### 8.4 Seed constraint (not seed encryption)
The seed need not be encrypted, but the seed's RHCOS ostree commit must ship an initramfs
containing the clevis/tpm2 dracut modules so the upgraded stateroot can unlock at boot. **Low
risk:** unlike stock RHEL (where you install `clevis-dracut` and regenerate the initramfs), RHCOS
integrates LUKS/clevis unlock into its initramfs via ignition-dracut and ships the modules by
default. Still worth a per-target-seed-version confirmation (spike S8, [Section 12](#12-test--validation-plan)).

### 8.5 Remaining empirical confirmation
Research indicates root unlock is driven by the **initramfs + `rd.luks.*`
kargs + the on-disk clevis token**, and that the deployed root's `/etc/crypttab` does not affect
root unlock ([Section 8.2](#82-what-actually-unlocks-root--and-what-the-pivot-drops)) — so the propagation target is the kargs. Confirm on a **real RHCOS-IBU node**
(spike S6, [Section 12](#12-test--validation-plan)): verify the exact `rd.luks.*` form RHCOS emits for an Ignition-encrypted root and
that re-attaching it to the new deployment yields unattended unlock. The same-machine TPM2 is
unchanged across the pivot, so no re-enrollment is expected with the no-PCR default.

## 9. Deferred: `/var/lib/containers` partition (residual risk)
Precached images stay **plaintext**. That partition is `mkfs`'d and populated by precache
*during the ISO install* (ibipreparation.go:88, ops.go:696), so first-boot Ignition
reprovisioning cannot protect its install-time contents without discarding the precache.
Encrypting it needs an **in-ISO cryptsetup** approach in `lca-cli` (luksFormat + `clevis luks
bind tpm2` before precache, `/etc/crypttab` into the stateroot) — larger, mock-touching
(`make generate`), proposed as **Phase 2**. Document that image content is readable from a
stolen disk until then.

## 10. Code change map

**lifecycle-agent — install path (`lca-cli ibi`):**
| File | Change |
|------|--------|
| `api/ibiconfig/ibiconfig.go` | Add `DiskEncryption` type + field on `IBIPrepareConfig`; extend `Validate()` (line 224). |
| `lca-cli/ibi-preparation/ignition.go` (new) | `buildRootLuksIgnition()` + marshal/merge helpers (vendored `ignition/v2/config/v3_4`). |
| `lca-cli/ibi-preparation/ibipreparation.go` | `writeRootLuksIgnition()`; add `--ignition-file` in `diskPreparation()` (line 188). |
| `lca-cli/ibi-preparation/ignition_test.go` (new) | Unit tests: config gen, PCR handling. |
| `lca-cli/ibi-preparation/ibipreparation_test.go` | Assert `--ignition-file` present when enabled, absent otherwise (~line 112). |

**lifecycle-agent — IBU pivot path (encryption must survive upgrade, [Section 8](#8-ibu-upgrade-interaction--required)):**
| File | Change |
|------|--------|
| `internal/prep/prep.go` | In `SetupStateroot` (`:93`, shared by IBU **and** install — [Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api)): detect booted-root LUKS; inject the node's `rd.luks.*` into the `kargs` slice before `Deploy` (`:183`), i.e. in the karg-build region (lines 173-181) — **the load-bearing step for root unlock**. No-op at install time (live-ISO root is not LUKS); gate on the `ibi bool` param (`:94`) if belt-and-suspenders is wanted. Only if non-root encrypted mounts exist, also write `/etc/crypttab` into the new `deploymentDir/etc` after the `etc.tgz` extraction (~206-215); not required for root ([Section 8.2](#82-what-actually-unlocks-root--and-what-the-pivot-drops)). |
| new helper (e.g. `internal/prep/` or `utils/`) | `isRootEncrypted()` + read current `rd.luks.*` kargs / `/etc/crypttab`. |
| `internal/prep/prep_test.go` | Assert `rd.luks.*` + crypttab propagated when booted root is LUKS; unchanged otherwise. |
| `lca-cli/ipconfig/prepivot.go` | **Required (kargs only).** The `IPConfig` CRD (separate flow) deploys the booted commit + reboots (`deployNewStateroot`, called at `:262-266`, defined at `:315-341`; kargs from MCO at `:147`) — same `rd.luks.*` drop risk, so call the shared detect+karg-propagate helper here. No crypttab injection needed: `copyEtc` (`:405-418`) already copies `/etc` forward from the running deployment. |

**openshift/installer (user-facing knob + contract):**
| File | Change |
|------|--------|
| `pkg/types/imagebased/imagebased_config_types.go` | Add `DiskEncryption` type + field on `InstallationConfig` (after the `CoreosInstallerArgs` field). Not on the sibling `Config` struct. |
| `pkg/asset/imagebased/image/ignition.go` | Add field to `ibiConfigurationFile` (lines 45-56) and map it in the populate block (lines 95-106) so it lands in `ibi-configuration.json`. **Load-bearing spot.** |
| `pkg/asset/imagebased/image/imagebased_config.go` | Add `validateDiskEncryption`, register in `validate()` (lines 165-206). |
| docs / template | Surface `diskEncryption` in the `image-based-installation-config.yaml` template + docs. |

**image-based-install-operator:** none.

## 11. Alternatives considered
- **`ImageBasedInstallationConfig.ignitionConfigOverride`** — rejected, two verified reasons:
  (1) it is parsed with `v3_2.Parse` (`ignition.go:162`), **hard-pinned to Ignition 3.2.0**,
  which predates `storage.luks` root reprovisioning (needs ≥ 3.3.0) and rejects a newer config;
  (2) it is merged only into the **live installer environment's** Ignition
  (`setIgnitionConfigOverride`, ignition.go:161-167) — used to pre-partition the disk — and is
  **never embedded into the installed system**; `coreos-installer` then overwrites root from the
  seed. It cannot reprovision the installed root.
- **`coreosInstallerArgs` passthrough** — rejected: the field is allow-listed to
  `--append-karg`/`--delete-karg`/`--save-partlabel`/`--save-partindex`, so `--ignition-file`
  (or any LUKS flag) cannot be passed. A first-class `diskEncryption` field is required.
- **Route through the IBI Operator / `ImageClusterInstall`** — impossible: Phase 2 only,
  post-write, no disk-write path ([Section 3](#3-verified-end-to-end-ibi-architecture-why-this-is-a-two-repo-change-not-an-ibi-operator-change)).
- **In-ISO cryptsetup for root by `lca-cli`** — rejected: reimplements grub/dracut/crypttab
  boot integration; high risk. (Reserved for the containers partition in Phase 2, where content
  timing forces it.)
- **Day-2 MachineConfig** — violates the "during installation, not post-deploy" requirement.

## 12. Test & validation plan
- **Unit:** Ignition generation (default TPM2, PCR list); `Validate()` in both repos;
  contract round-trip (installer JSON → `IBIPrepareConfig`); install-args assembly; **IBU
  propagation** — `rd.luks.*` added to the deploy kargs when the booted root is LUKS (and not
  touched when it is not), and `/etc/crypttab` carried into the new deployment only when non-root
  encrypted mounts exist ([Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api)).
- **Spikes / e2e** — a virtual TPM may be used for early functional iteration, but the
  supported configuration is a **discrete hardware TPM2 ([Section 7](#7-tpm2-binding-policy))**, so final validation of every spike
  below must run on real hardware with a discrete TPM. Stable labels `S1`–`S8`:
  - **S1 — First-boot reprovisioning actually fires** on an IBI-installed node given `lca-cli`'s
    post-`coreos-installer` seed deploy + `cleanupRhcosSysroot()` — verify root comes up as
    `/dev/mapper/root` (LUKS2), TPM2-unlocked, unattended. **Highest-risk install item.**
  - **S2 — RAM headroom** for staging the root FS (excluding `/var/lib/containers`) on target SKUs.
  - **S3 — TPM2 binding present:** `clevis luks list -d /dev/disk/by-partlabel/root` shows the
    TPM2 binding; reboot loop confirms deterministic unattended unlock.
  - **S4 — Negative:** disk moved to another machine fails to unlock.
  - **S5 — Regression:** `diskEncryption` unset ⇒ byte-identical behavior to today.
  - **S6 — Root-unlock mechanism ([Section 8.5](#85-remaining-empirical-confirmation)):** research indicates kargs are load-bearing and the
    deployed `/etc/crypttab` is not ([Section 8.2](#82-what-actually-unlocks-root--and-what-the-pivot-drops)); confirm on a real RHCOS-IBU node the exact `rd.luks.*`
    form RHCOS emits for an Ignition-encrypted root and that re-attaching it to a new deployment
    yields unattended unlock.
  - **S7 — IBU-on-encrypted-node end-to-end ([Section 8](#8-ibu-upgrade-interaction--required), required):** upgrade an encrypted SNO; confirm
    the new stateroot boots and TPM2-unlocks root unattended, no passphrase, no re-enrollment.
    Include a Rollback: the previous (still-encrypted) deployment must also auto-unlock.
  - **S8 — Seed/RHCOS build check ([Section 8.4](#84-seed-constraint-not-seed-encryption), [Section 2](#2-background-how-rhcos-tpm2-root-luks-works)):** confirm the target seed's RHCOS initramfs carries
    the clevis/tpm2 dracut modules and ships an Ignition new enough for root reprovisioning (≥3.3).

## 13. Phasing & effort
- **Phase 1 (this doc) — install-time encryption:** root-FS TPM2 via native Ignition.
  lifecycle-agent: ~1 new file + 2 edits + tests. openshift/installer: type + 1 contract mapping
  + validator + template. Effort dominated by spike S1 and hardware validation. Sequence: land +
  test the `lca-cli` engine first (works via standalone `lca-cli ibi -f`), then the
  openshift/installer knob to expose it.
- **Phase 1b (this doc) — IBU survival (REQUIRED, ships with the feature):** propagate the
  node's unlock config across the pivot ([Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api)) — `rd.luks.*` karg injection in **both** the IBU
  Prep path and the `IPConfig` pivot path + a shared detection helper + unit tests; plus
  conditional non-root `/etc/crypttab` carry-forward in the Prep path only ([Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api)). Gated on spikes
  S6/S7. This is *not* optional and *not* deferred; an encrypted node that can't be upgraded is not
  shippable. Depends on Phase 1 having produced a real encrypted node to test against.
- **Phase 2:** `/var/lib/containers` in-ISO cryptsetup (new `ops` methods, `make generate`).
- **Phase 3:** recovery-key escrow.

## 14. Open questions
The design questions are answered inline in the sections above (architecture [Section 3](#3-verified-end-to-end-ibi-architecture-why-this-is-a-two-repo-change-not-an-ibi-operator-change); root-unlock
mechanism [Section 8.2](#82-what-actually-unlocks-root--and-what-the-pivot-drops)/[Section 8.5](#85-remaining-empirical-confirmation); seed constraint [Section 8.4](#84-seed-constraint-not-seed-encryption); encryption detection [Section 8.3](#83-design-propagate-the-running-nodes-unlock-config-no-new-ibu-api); version/RAM requirements
[Section 2](#2-background-how-rhcos-tpm2-root-luks-works)). What remains is purely **empirical validation**, each tracked as a spike in [Section 12](#12-test--validation-plan):

1. **Does first-boot Ignition reprovisioning actually fire** after `lca-cli`'s seed redeploy?
   Highest-risk install item. (spike **S1**)
2. **What exact `rd.luks.*` form does RHCOS emit** for an Ignition-encrypted root, and does an
   **encrypted node upgrade end-to-end** — new stateroot TPM2-unlocks unattended, and a rollback
   to the previous (still-encrypted) deployment also auto-unlocks? (spikes **S6**, **S7**)
3. **Does the target seed/RHCOS build** ship the clevis/tpm2 initramfs modules and an Ignition
   new enough (≥3.3) for root reprovisioning? (spike **S8**)
