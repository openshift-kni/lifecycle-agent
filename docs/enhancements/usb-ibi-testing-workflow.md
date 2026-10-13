# USB-IBI Testing Workflow — Findings & Decision Note

> **Status:** Spike / investigation in progress (CNF-27197). Milestone 1 (tooling + this note) is
> delivered by this PR. Milestone 2 (the hardware findings) is completed on lab hardware and recorded
> below. Sections marked **TODO: complete on lab hardware** are filled in during milestone 2.
> **Jira:** [CNF-27197](https://redhat.atlassian.net/browse/CNF-27197) — *[Spike] Establish an initial
> testing workflow for USB-IBI (real lab hardware vs. virtual media)*.
> **Design reference:** [USB-Based IBI for Disconnected SNO](https://github.com/openshift-kni/lifecycle-agent/pull/8693)
> (`docs/enhancements/usb-ibi.md`), §4 (media layout), §5.2, §9 (phasing), §10 (open questions).

## Purpose

Testing the USB-IBI feature is hard: it needs real USB devices and real lab hardware (UEFI Secure Boot,
GNR-D reference platforms), and several stories cannot be validated until bootable media can be assembled
and booted. This note decides **how** we test the prepare/reconfigure and `create-usb` flows before and
during implementation, and answers the three questions raised in the review of the epic (CNF-26900):

1. **Lab-hardware logistics** — what hardware, how we access it, and the manual boot-and-observe loop.
2. **Bootstrapping testable media (the chicken-and-egg)** — how we produce the initially assembled test
   images before the `create-usb` stories land.
3. **Virtual media as an accelerator** — whether the whole USB output can be attached over the BMC
   instead of a physical stick, plus the reviewer-suggested in-place write path.

This is an **enabler** spike. Its output — the bootstrapped test images plus this decision — unblocks the
bootability gate (CNF-27158) and the end-to-end validation (CNF-27174).

## Spike completion criteria (two milestones)

This spike is delivered in two milestones. **The PR that adds this note and the `hack/` tooling is
milestone 1 and does NOT close CNF-27197.**

- **Milestone 1 — tooling + this note (agent-deliverable, this PR).** The throwaway media-assembly
  script (`hack/usb-ibi-assemble-media.sh`), the in-place-write helper (`hack/usb-ibi-write-inplace.sh`),
  and this findings note with the hardware-dependent results stubbed as **TODO: complete on lab
  hardware**. Merges when lint and the argument-path smoke checks pass.
- **Milestone 2 — hardware findings (human, keeps the spike open).** On the CNF-27219 lab server with the
  CNF-27218 USB, record below: (a) assembled media boots on reference GNR-D (physical); (b) Secure Boot
  status observed and the prepare flow driven to observable progress; (c) BMC virtual-media (Redfish
  `VirtualMedia`) attach of the whole `.img` boots and enumerates the data partitions; (d) in-place `dd` +
  one-shot boot into live works under Secure Boot. Then state the **recommended initial workflow** and the
  hand-off to CNF-27174.
- **Owner of the hardware runs:** Steven Skeard (developer of record). The spike stays **open** until the
  milestone-2 findings are recorded here and the recommended workflow is stated.

## 1. Lab-hardware setup & BMC access

Reference GNR-D (Granite Rapids-D) platforms from Dell and HPE are the target hardware for USB-IBI
testing. The provisioning dependencies for this spike are both satisfied:

- **Lab server** — CNF-27219 (*Secure a lab server (reference hardware) for USB-IBI testing*), **Closed**.
- **Physical USB** — CNF-27218 (*Obtain a physical USB device in the lab for USB-IBI testing*), **Closed**.

BMC access per vendor:

- **Dell** — iDRAC (Redfish at `https://<idrac>/redfish/v1/`). Virtual media and one-time boot via the
  iDRAC UI or Redfish `VirtualMedia` / `Boot` overrides.
- **HPE** — iLO (Redfish at `https://<ilo>/redfish/v1/`). Virtual media and one-time boot via the iLO UI
  or Redfish.

| Item | Value |
| --- | --- |
| Dell host / iDRAC address | **TODO: complete on lab hardware** |
| HPE host / iLO address | **TODO: complete on lab hardware** |
| BMC credentials location | **TODO: complete on lab hardware** (vault / lab doc reference) |
| Serial-over-LAN / console access | **TODO: complete on lab hardware** |

## 2. Producing the initial test images (the chicken-and-egg)

`create-usb` does not exist in `main` yet — it is only proposed in PR #8693. To boot-test media **before**
that tooling lands, this spike owns producing the initial test images with throwaway tooling:
`hack/usb-ibi-assemble-media.sh`.

### 2.1 Precise media layout rule

The on-node code discovers data partitions **by filesystem label**, not by GPT partition number (proposal
§4: `ibi-images` is found by label; `post-pivot`'s `waitForConfiguration` scans for the `cluster-config`
label; `ibi-status` is found by label). The numbering rule follows from that:

- **GPT partitions are numbered contiguously 1..N in on-disk order.** The `pN` names in the proposal are
  **logical roles identified by filesystem label**, not fixed GPT partition numbers.
- **Self-contained** (data partitions, in on-disk order): `ibi-images`, then `cluster-config`, then
  `ibi-status`, appended after the live ISO.
- **Install-only** (no `cluster-config`): `ibi-images`, then `ibi-status`, appended after the live ISO.
  The writable status partition is still discovered by its `ibi-status` label, so nothing on-node cares
  where it lands in GPT order relative to the self-contained layout.
- The script addresses partitions **by label after creation**, never by a hard-coded number, so both
  profiles use one code path.
- **Caveat — the live ISO is not a single GPT partition.** A real RHCOS isohybrid live ISO contributes
  **more than one** GPT partition (the ISO9660 filesystem plus an EFI System Partition), so the appended
  data partitions do **not** start at GPT index 2. The "GPT N" column below is the *logical role order*
  for readability only. The assembly script therefore sets a GPT partition **name** on each appended
  partition (`sgdisk --change-name`) and resolves the node by that name before running `mkfs` — it never
  assumes a fixed index, which is what keeps it from clobbering the ISO's EFI partition.

| Role (logical order) | Label | FS | Self-contained | Install-only | Contents |
| --- | --- | --- | --- | --- | --- |
| live ISO | — (ISO9660/EFI) | — | first (1+ GPT parts) | first (1+ GPT parts) | RHCOS live ISO + embedded ignition that auto-runs `lca-cli ibi -f <cfg>` |
| images | `ibi-images` | xfs | appended | appended | OCI image layout (seed + referenced images) |
| config | `cluster-config` | ext4 | appended | — | `/opt/openshift/...` reconfigure tree |
| status | `ibi-status` | ext4 | appended | appended | per-phase records + logs (writable, journaled) |

### 2.2 Append method — layout (a) "append after the isohybrid ISO"

The script implements candidate layout **(a)** because it has RHCOS precedent. The concrete steps:

1. Write the RHCOS live ISO to the front of the image/stick as an isohybrid (`coreos-installer iso
   ignition embed` to auto-run `lca-cli ibi -f <cfg>`, then `dd` the ISO to the target). This lays down
   the ISO's own protective MBR / EFI (El Torito) entries.
2. Grow the target and move the backup GPT header to the end of the larger device
   (`sgdisk --move-second-header`).
3. Append the data partitions in the free space **after** the ISO region with `sgdisk --new=0:0:+<size>`
   (next free number, first-free-sector, sized), in order `ibi-images` → [`cluster-config`] →
   `ibi-status`, and set a GPT partition **name** on each with `sgdisk --change-name` so the node can be
   located without assuming an index.
4. Resolve each appended partition by its GPT name, then `mkfs` it and **set the FS label** to the exact
   strings `ibi-images` / `cluster-config` / `ibi-status` (`mkfs.xfs -L` / `mkfs.ext4 -L`). The label is
   the contract, so this step is mandatory and validated.
5. Mount **by label**, populate (`skopeo copy` / OCI layout into `ibi-images`; the `/opt/openshift/...`
   tree into `cluster-config` for self-contained; leave `ibi-status` empty), unmount, `sync`.
6. `sgdisk --verify` + `partprobe`, and assert every expected label resolves.

Layouts **(b)** fresh-partitioned and **(c)** files-in-ISO remain **documented, un-implemented**
alternatives for the hardware spike to choose between — (a) is the default. Implementing all three up
front is wasted throwaway effort; the hardware run decides whether (a) is sufficient or (b)/(c) is needed.

### 2.3 Invoking the bootstrap script

```bash
# Self-contained media (ISO + ibi-images + cluster-config + ibi-status):
sudo hack/usb-ibi-assemble-media.sh \
    --profile self-contained \
    --iso rhcos-live.iso \
    --output usb-ibi.img \
    --ibi-images ./oci-layout \
    --cluster-config ./opt-openshift \
    --ignition ./ibi.ign

# Install-only media (ISO + ibi-images + ibi-status; no cluster-config):
sudo hack/usb-ibi-assemble-media.sh \
    --profile install-only \
    --iso rhcos-live.iso \
    --output usb-ibi.img \
    --ibi-images ./oci-layout \
    --ignition ./ibi.ign
```

Both examples pass `--ignition ./ibi.ign` so a pristine RHCOS live ISO auto-runs `lca-cli ibi` on
boot (§3, step 3). Omit it only when the ISO already carries an embedded ignition config.

The output `usb-ibi.img` is then either written to a physical stick, written in place (§5), or attached as
BMC virtual media (§4).

| Media-assembly finding | Result |
| --- | --- |
| Layout (a) assembles cleanly and all labels resolve | **TODO: complete on lab hardware** |
| Whether (b) or (c) is needed instead | **TODO: complete on lab hardware** |

## 3. Manual boot-and-observe loop

The minimum manual workflow to boot assembled media and observe progress:

1. Attach the media (physical stick inserted, in-place write, or BMC virtual media).
2. Select the USB / live media as a **one-time boot device** (BMC one-time boot or firmware boot menu).
3. Firmware boots the live ISO (p1); the embedded ignition auto-runs `lca-cli ibi -f <cfg>`.
4. Observe the **prepare** phase (image import from `ibi-images`) and, for self-contained, the
   **reconfigure** phase on first boot.
5. Read the results off-box: the `ibi-status` partition holds `prepare.json` (and `reconfigure.json` for
   self-contained) plus logs. With no console/network during install, this partition is the record of what
   happened — mount it by label from another host to read it.

| Observation | Result |
| --- | --- |
| Media boots on reference GNR-D (physical) | **TODO: complete on lab hardware** |
| Secure Boot status observed | **TODO: complete on lab hardware** |
| Prepare flow driven to observable progress | **TODO: complete on lab hardware** |
| `ibi-status` readable off-box | **TODO: complete on lab hardware** |

## 4. Virtual-media feasibility procedure

Evaluate whether the whole USB output (the single `.img`) can be attached to lab hardware as **virtual
media** over the BMC instead of a physical stick.

Procedure:

1. Host `usb-ibi.img` on an HTTP(S) server reachable by the BMC.
2. Attach it as virtual media:
   - **Dell/iDRAC** — Redfish `VirtualMedia` `InsertMedia`, or the iDRAC virtual-media UI.
   - **HPE/iLO** — Redfish `VirtualMedia` `InsertMedia`, or the iLO virtual-media UI.
3. Set the virtual media as the one-time boot device and power-cycle.
4. Verify the isohybrid p1 boots and that the data partitions (`ibi-images`, `cluster-config` where
   present, `ibi-status`) **enumerate** the same as on a physical stick, under Secure Boot.
5. Record size limits and performance (large virtual disks over the BMC can be slow or capped).

| Virtual-media finding | Result |
| --- | --- |
| BMC exposes a large-enough virtual USB/disk | **TODO: complete on lab hardware** |
| Isohybrid p1 boots as virtual media | **TODO: complete on lab hardware** |
| Data partitions enumerate the same as a physical stick | **TODO: complete on lab hardware** |
| Size limits / performance cost | **TODO: complete on lab hardware** |

## 5. In-place write procedure (reduce lab round-trips)

Reviewer-suggested time-saver (Steven Skeard, CNF-27197): leave a USB stick **permanently plugged into the
target host** and write the assembled image to it **from the running base OS** (over SSH), then one-shot
boot into the live environment. This avoids the slow loop of asking the lab team to physically create and
insert sticks.

`hack/usb-ibi-write-inplace.sh` implements this safely. **A wrong `dd` target can destroy the lab host**,
so write and reboot are **separate** sub-commands and every write is guarded:

- target must be a stable `/dev/disk/by-id/...` path resolving to a real block device;
- target must be **removable** and **not** the running system/root disk;
- target must have **no mounted partitions**;
- the image must **fit** on the target (checked before any write);
- the resolved device (model/serial/size) must be confirmed (`--yes` or interactive prompt);
- `--dry-run` runs all checks and prints the exact `dd` without writing — run this first.

```bash
# 1. Dry-run first (validates, writes nothing):
sudo hack/usb-ibi-write-inplace.sh write \
    --image usb-ibi.img --target /dev/disk/by-id/usb-<id> --dry-run

# 2. Write (separate, explicit step; prompts for confirmation):
sudo hack/usb-ibi-write-inplace.sh write \
    --image usb-ibi.img --target /dev/disk/by-id/usb-<id>

# 3. Arm a one-shot boot into the live media and reboot (separate step):
sudo hack/usb-ibi-write-inplace.sh arm-boot --target /dev/disk/by-id/usb-<id>
```

| In-place-write finding | Result |
| --- | --- |
| `dd` in place from the base OS succeeds | **TODO: complete on lab hardware** |
| One-shot boot into live works under Secure Boot | **TODO: complete on lab hardware** |
| Net time saved vs. physical/virtual media | **TODO: complete on lab hardware** |

## 6. Recommended initial workflow & hand-off to CNF-27174

The recommended initial testing workflow is decided after the milestone-2 hardware findings above are
recorded. It states what runs on physical media vs. virtual media, what is manual vs. automated, and the
hand-off to the automated end-to-end work in CNF-27174.

- **Recommended initial workflow (physical vs. virtual, manual vs. automated):** **TODO: complete on lab
  hardware** (decide after §3–§5 findings).
- **Hand-off to CNF-27174 (e2e disconnected install validation):** **TODO: complete on lab hardware**
  (which steps the e2e automation inherits, and what stays manual).

## Dependencies

- **Depends on** CNF-27218 (physical USB, **Closed**) and CNF-27219 (lab server, **Closed**).
- **Produces** the initially assembled test media that CNF-27158 (bootability / media-assembly gate)
  consumes to run its Secure Boot proof; CNF-27158 feeds its assembly-approach decision back here.
- **Enables** CNF-27174 (end-to-end disconnected install validation).
