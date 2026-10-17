#!/bin/bash
#
# usb-ibi-write-inplace.sh - safely write assembled USB-IBI media to a stick that
# is left permanently plugged into the target lab host, from the running base OS,
# then one-shot boot into it (CNF-27197).
#
# This is the reviewer-suggested time-saver (Steven Skeard, CNF-27197): instead
# of the slow loop of asking the lab team to physically create and insert sticks,
# leave one stick plugged in and (re)write it over SSH, then one-shot boot.
#
# A wrong `dd` target can destroy the lab host, so the design is deliberately
# conservative:
#   * TWO separate sub-commands - writing never auto-reboots:
#       write    - validate, then dd the image to the target
#       arm-boot - arm a one-shot UEFI boot into the live media, then reboot
#   * mandatory pre-write safety checks (all must pass):
#       - target is a /dev/disk/by-id/... path resolving to a real block device
#       - target is removable and is NOT the running system/root disk
#       - target has no mounted partitions
#       - the image fits on the target (size check before any write)
#       - the resolved device (model/serial/size) is confirmed (--yes or prompt)
#   * --dry-run validates and prints the exact dd it would run, writing nothing.
#
# This is throwaway spike tooling - not a product feature.
#

set -euo pipefail

PROG=$(basename "$0")

declare SUBCMD=
declare IMAGE=
declare TARGET=
declare DRY_RUN=false
declare ASSUME_YES=false

function usage {
    cat <<EOF
Usage:
    ${PROG} write --image <media.img> --target /dev/disk/by-id/<id> [--dry-run] [--yes]
    ${PROG} arm-boot --target /dev/disk/by-id/<id> [--dry-run] [--yes]
    ${PROG} --help

Sub-commands:
    write       Validate the target, then write <media.img> to it with dd.
                Does NOT reboot. Run 'arm-boot' separately afterwards.
    arm-boot    Arm a one-shot UEFI boot into the live media (efibootmgr
                --bootnext) and reboot the host. Separate from 'write' on
                purpose - you decide when the host reboots.

Options:
    --image     Assembled media image to write (required for 'write').
    --target    Block device to write / boot, given as a STABLE
                /dev/disk/by-id/... path (required). by-id is used because it is
                stable across reboots and device renumbering.
    --dry-run   Run all validation and print the exact dd / efibootmgr commands,
                but write nothing and do not reboot. Recommended first run.
    --yes       Skip the interactive confirmation (for scripted/remote use).
                The safety checks still run - only the final prompt is skipped.
    -h, --help  Show this help and exit.

Safety: a wrong target destroys data. 'write' refuses any target that is not a
removable, non-system, unmounted device large enough for the image. Always try
--dry-run first.
EOF
    exit "${1:-1}"
}

function die {
    echo "${PROG}: error: $*" >&2
    exit 1
}

function require_cmd {
    local cmd
    for cmd in "$@"; do
        command -v "${cmd}" >/dev/null 2>&1 || die "required command not found: ${cmd}"
    done
}

# resolve_target resolves the by-id path to its canonical /dev node and prints
# the base device name (e.g. sdb), failing if the path is not a by-id symlink to
# a real WHOLE-DISK block device. Partition targets are rejected so the device we
# validate is exactly the device we later write the whole image to.
function resolve_target {
    local target="$1"
    case "${target}" in
        /dev/disk/by-id/*) ;;
        *) die "target must be a /dev/disk/by-id/... path (got: ${target})" ;;
    esac
    [ -e "${target}" ] || die "target path does not exist: ${target}"

    local resolved
    resolved=$(readlink -f "${target}") || die "could not resolve ${target}"
    [ -b "${resolved}" ] || die "target does not resolve to a block device: ${resolved}"

    local devtype
    devtype=$(lsblk -ndo TYPE "${resolved}" 2>/dev/null || true)
    [ "${devtype}" = "disk" ] || \
        die "target must be a whole disk, not a partition or ${devtype:-unknown}: ${target}"

    basename "${resolved}"
}

# system_disks prints the physical whole-disk device(s) backing the running root
# filesystem, so we can refuse to ever write to one. It walks the dependency
# stack downward, so plain partitions, LVM, LUKS and md roots all resolve to the
# underlying disk(s).
function system_disks {
    local root_src
    root_src=$(findmnt -no SOURCE / 2>/dev/null || true)
    [ -n "${root_src}" ] || return 0
    lsblk -nrso NAME,TYPE "${root_src}" 2>/dev/null | awk '$2 == "disk" { print $1 }'
}

# safety_checks runs every mandatory pre-write guard against the base device.
# Any failure aborts before a single byte is written.
function safety_checks {
    local base="$1"
    local dev="/dev/${base}"
    [ -b "${dev}" ] || die "resolved device is not a block device: ${dev}"

    # Guard 1: must be removable.
    local removable
    removable=$(cat "/sys/block/${base}/removable" 2>/dev/null || echo 0)
    [ "${removable}" = "1" ] || \
        die "refusing: ${dev} is not removable (/sys/block/${base}/removable=${removable})"

    # Guard 2: must not be a physical disk backing the running root filesystem.
    local sys
    while read -r sys; do
        [ -n "${sys}" ] || continue
        if [ "${sys}" = "${base}" ]; then
            die "refusing: ${dev} backs the running root filesystem (system disk)"
        fi
    done < <(system_disks)

    # Guard 3: must have no mounted partitions (the whole-disk node or any child).
    local mounted
    mounted=$(lsblk -nro NAME,MOUNTPOINT "${dev}" | awk 'NF>1 {print $1" -> "$2}')
    if [ -n "${mounted}" ]; then
        die "refusing: ${dev} has mounted partitions:"$'\n'"${mounted}"
    fi
}

# image_fits fails fast if the image is larger than the target device.
function image_fits {
    local image="$1" base="$2"
    local image_bytes dev_bytes
    image_bytes=$(stat -c %s "${image}")
    dev_bytes=$(blockdev --getsize64 "/dev/${base}")
    if [ "${image_bytes}" -gt "${dev_bytes}" ]; then
        die "image (${image_bytes} bytes) does not fit on /dev/${base} (${dev_bytes} bytes)"
    fi
    echo "    image size : ${image_bytes} bytes"
    echo "    device size: ${dev_bytes} bytes (fits)"
}

# describe prints the resolved device identity so the operator can confirm the
# right stick before anything destructive happens.
function describe {
    local base="$1"
    echo "==> Resolved target device:"
    lsblk -do NAME,MODEL,SERIAL,SIZE,RM,TYPE "/dev/${base}" || true
}

# confirm requires explicit agreement unless --yes was given.
function confirm {
    local base="$1"
    if [ "${ASSUME_YES}" = "true" ]; then
        return 0
    fi
    local reply
    read -r -p "Type the device name '${base}' to confirm writing to /dev/${base}: " reply
    [ "${reply}" = "${base}" ] || die "confirmation did not match; aborting"
}

function cmd_write {
    [ -n "${IMAGE}" ] || { echo "${PROG}: --image is required for 'write'" >&2; usage 1; }
    [ -n "${TARGET}" ] || { echo "${PROG}: --target is required for 'write'" >&2; usage 1; }
    [ -f "${IMAGE}" ] || die "image not found: ${IMAGE}"
    # partprobe is absent on the RHCOS base OS, so the table reread uses
    # 'blockdev --rereadpt'. sgdisk is only needed for a GPT image (required
    # lazily inside fixup_table), because the media this tooling assembles is now
    # DOS/MBR. (fontivan / DoR)
    require_cmd dd sync stat lsblk blkid readlink findmnt blockdev awk sfdisk udevadm

    if [ "$(id -u)" -ne 0 ] && [ "${DRY_RUN}" != "true" ]; then
        die "must run as root to write to a block device"
    fi

    local base
    base=$(resolve_target "${TARGET}")
    describe "${base}"
    safety_checks "${base}"
    image_fits "${IMAGE}" "${base}"

    local dd_cmd="dd if=${IMAGE} of=${TARGET} bs=4M conv=fsync status=progress"
    if [ "${DRY_RUN}" = "true" ]; then
        local img_pttype
        img_pttype=$(blkid -s PTTYPE -o value "${IMAGE}" 2>/dev/null || true)
        echo "==> [dry-run] all safety checks passed. Would run:"
        echo "    ${dd_cmd}"
        echo "    sync"
        case "${img_pttype}" in
            gpt)
                echo "    sgdisk --move-second-header ${TARGET}        # relocate backup GPT to device end"
                echo "    blockdev --rereadpt ${TARGET} && udevadm settle"
                echo "    sgdisk --verify ${TARGET}                    # confirm the GPT is valid"
                ;;
            dos)
                echo "    blockdev --rereadpt ${TARGET} && udevadm settle   # partprobe is absent on RHCOS"
                echo "    sfdisk --verify ${TARGET}                    # validate the MBR table (ISO's expected p1/p2 overlap tolerated)"
                echo "    # confirm the ibi-images / ibi-status FS labels resolve on ${TARGET}"
                ;;
            *)
                echo "    # WARNING: could not detect a GPT or DOS table on ${IMAGE} (PTTYPE='${img_pttype:-<none>}')"
                ;;
        esac
        return 0
    fi

    confirm "${base}"
    echo "==> Writing ${IMAGE} to ${TARGET}"
    dd if="${IMAGE}" of="${TARGET}" bs=4M conv=fsync status=progress
    sync
    fixup_table "${base}"
    echo "==> Write complete. Run '${PROG} arm-boot --target ${TARGET}' to one-shot boot into it."
}

# reread_table asks the kernel to re-read the device's partition table and waits
# for the nodes to settle. partprobe is absent on the RHCOS base OS, so the reread
# uses 'blockdev --rereadpt'. A failed reread means the kernel may still expose the
# stale layout, so it is treated as a write failure. (fontivan / DoR; CodeRabbit)
function reread_table {
    local dev="$1"
    blockdev --rereadpt "${dev}" || die "could not reread partition table on ${dev}"
    udevadm settle 2>/dev/null || true
}

# verify_mbr_table runs 'sfdisk --verify' but tolerates the one overlap that is
# inherent to a RHCOS isohybrid ISO carried onto the stick: its partition 1 (the
# whole ISO9660 image) deliberately contains partition 2 (the ESP), so sfdisk
# always reports "Partition 1: overlaps partition 2" for the untouched ISO
# entries. sfdisk then counts that overlap in a trailing "N error(s) detected."
# summary and exits non-zero. Both the overlap line and the summary it produces
# are expected and harmless - the appended data partitions sit after the ISO and
# overlap nothing - so we strip both before judging the rest, and fail only if
# sfdisk reports (or counts) anything beyond the expected overlap. This mirrors
# the post-assembly verifier in usb-ibi-assemble-media.sh so both the image and
# the written stick accept the exact same "overlap + 1 error detected." output.
# (fontivan / Developer of Record)
function verify_mbr_table {
    local dev="$1"
    local out rc=0
    out=$(sfdisk --verify "${dev}" 2>&1) || rc=$?

    # Count the expected ISO p1/p2 overlap(s) and the total errors sfdisk reported
    # in its "N error(s) detected." summary. The summary counts the expected
    # overlap, so comparing the two counts - rather than scanning for the word
    # "error" - is what lets us tolerate "1 error detected." on valid media while
    # still failing on anything sfdisk counts beyond the expected overlap.
    local expected_overlaps detected
    expected_overlaps=$(printf '%s\n' "${out}" | grep -cE 'Partition 1: overlaps partition 2\.?$')
    detected=$(printf '%s\n' "${out}" | sed -nE 's/^[[:space:]]*([0-9]+) error[s]? detected.*/\1/p' | tail -n1)
    : "${detected:=0}"

    # Any overlap sfdisk reports other than the expected ISO p1/p2 overlap is
    # fatal. (sfdisk's own "...no partition is overlapped." progress line is not a
    # finding, so it is excluded.)
    local other_overlaps
    other_overlaps=$(printf '%s\n' "${out}" \
        | grep -iE 'overlap' \
        | grep -vE 'Partition 1: overlaps partition 2\.?$' \
        | grep -ivE 'no partition is overlapped' || true)
    if [ -n "${other_overlaps}" ]; then
        printf '%s\n' "${out}" >&2
        die "partition table verification failed on ${dev} after write (unexpected overlap)"
    fi

    # Every error sfdisk counted must be one of the expected ISO overlaps; any
    # extra error - or a non-zero exit we cannot attribute to the overlap - is fatal.
    if [ "${detected}" -gt "${expected_overlaps}" ]; then
        printf '%s\n' "${out}" >&2
        die "partition table verification failed on ${dev} after write (${detected} error(s) detected, ${expected_overlaps} expected)"
    fi
    if [ "${rc}" -ne 0 ] && [ "${detected}" -eq 0 ] && [ "${expected_overlaps}" -eq 0 ]; then
        printf '%s\n' "${out}" >&2
        die "partition table verification failed on ${dev} after write (sfdisk exit ${rc})"
    fi

    if [ "${expected_overlaps}" -gt 0 ]; then
        echo "    ok: partition table verified (ISO's expected p1/p2 hybrid overlap ignored)"
    else
        echo "    ok: partition table verified"
    fi
}

# fixup_table makes the freshly written stick's partition table valid for its
# label type. dd copies the image byte-for-byte, and image_fits only guarantees
# the image is no larger than the target, so:
#   * GPT  - when the stick is bigger than the image the backup GPT header lands
#            partway into the device instead of at its last sector (an invalid
#            secondary GPT). 'sgdisk --move-second-header' relocates it to the real
#            end (a no-op when sizes match), then we verify it.
#   * DOS/MBR - what usb-ibi-assemble-media.sh now produces. The MBR table lives at
#            sector 0 and is valid regardless of device size; there is NO secondary
#            header to relocate, so GPT relocation and 'sgdisk --verify' must NOT
#            run on it. We validate the MBR table and the data-partition labels
#            instead. (fontivan / DoR)
# In both cases the kernel reread uses reread_table (blockdev --rereadpt).
function fixup_table {
    local base="$1"
    local dev="/dev/${base}"
    local pttype
    pttype=$(blkid -s PTTYPE -o value "${dev}" 2>/dev/null || true)
    case "${pttype}" in
        gpt)
            require_cmd sgdisk
            echo "==> GPT target: relocating backup header to the end of ${dev} and verifying"
            sgdisk --move-second-header "${dev}" || die "could not relocate GPT backup header on ${dev}"
            reread_table "${dev}"
            # 'sgdisk --verify' only checks the on-disk GPT; the reread above is
            # what guarantees the kernel map is not stale. (CodeRabbit review)
            sgdisk --verify "${dev}" || die "GPT verification failed on ${dev} after write"
            ;;
        dos)
            echo "==> DOS/MBR target: re-reading and validating the partition table on ${dev}"
            reread_table "${dev}"
            verify_mbr_table "${dev}"
            verify_contract_labels "${base}"
            ;;
        *)
            die "unrecognized partition table type on ${dev} (PTTYPE='${pttype:-<none>}'); refusing to validate the write"
            ;;
    esac
}

# verify_contract_labels confirms the data partitions survived the write by
# resolving the contract filesystem labels among the TARGET device's own
# partitions (never a global 'blkid -L', which could match another disk). Both
# media profiles carry ibi-images and ibi-status; cluster-config is self-contained
# only and therefore not required here.
function verify_contract_labels {
    local base="$1"
    local label dev fslabel found
    for label in ibi-images ibi-status; do
        found=
        while read -r dev; do
            [ -b "${dev}" ] || continue
            fslabel=$(blkid -s LABEL -o value "${dev}" 2>/dev/null || true)
            if [ "${fslabel}" = "${label}" ]; then
                found="${dev}"
                break
            fi
        done < <(lsblk -rno NAME,TYPE "/dev/${base}" | awk '$2 == "part" { print "/dev/"$1 }')
        [ -n "${found}" ] || \
            die "expected data-partition label '${label}' not found on /dev/${base} after write"
        echo "    ok: ${label} -> ${found}"
    done
}

# entry_matches_target returns success only if the given efibootmgr boot entry's
# device path points at the target disk /dev/${base}. A four-hex-digit entry can
# equally name the host disk, a NIC/PXE path, or another device, so the format
# check alone proves nothing. 'efibootmgr -v' prints each entry's device path with
# an HD(...) node that carries either a GPT partition UUID or, for a DOS/MBR disk
# (what this tooling now assembles), the 32-bit MBR disk signature, e.g.
#   Boot0003* RHCOS  HD(1,GPT,2b2f...-...,0x800,0x64000)/File(\EFI\BOOT\BOOTX64.EFI)
#   Boot0004* RHCOS  HD(1,MBR,0x0980183f,0x800,0x64000)/File(\EFI\BOOT\BOOTX64.EFI)
# We extract whichever identifier is present and match it (case-insensitively)
# against the target:
#   * GPT -> the PARTUUIDs of /dev/${base}'s partitions
#   * MBR -> the disk's PTUUID (its MBR signature)
# If no identifier can be extracted, or none matches, the match cannot be
# established and we fail so the caller aborts.
function entry_matches_target {
    local entry="$1" base="$2"
    local line

    line=$(efibootmgr -v | grep -iE "^Boot${entry}\*? " || true)
    [ -n "${line}" ] || return 1

    # GPT partition-UUID form: HD(<n>,GPT,<uuid>,...).
    local gpt_uuid u
    gpt_uuid=$(printf '%s\n' "${line}" \
        | grep -ioE 'HD\([0-9]+,GPT,[0-9a-fA-F-]+' \
        | head -n1 \
        | sed -E 's/.*,GPT,//')
    if [ -n "${gpt_uuid}" ]; then
        while read -r u; do
            [ -n "${u}" ] || continue
            [ "${u,,}" = "${gpt_uuid,,}" ] && return 0
        done < <(lsblk -nro PARTUUID "/dev/${base}" 2>/dev/null || true)
        return 1
    fi

    # MBR disk-signature form: HD(<n>,MBR,0x<sig>,...). blkid prints the PTUUID
    # without the leading 0x, so normalize both before comparing.
    local mbr_sig ptuuid
    mbr_sig=$(printf '%s\n' "${line}" \
        | grep -ioE 'HD\([0-9]+,MBR,0x[0-9a-fA-F]+' \
        | head -n1 \
        | sed -E 's/.*,MBR,0x//')
    if [ -n "${mbr_sig}" ]; then
        ptuuid=$(blkid -s PTUUID -o value "/dev/${base}" 2>/dev/null | sed -E 's/^0x//')
        [ -n "${ptuuid}" ] || return 1
        [ "${mbr_sig,,}" = "${ptuuid,,}" ] && return 0
        return 1
    fi

    return 1
}

function cmd_arm_boot {
    [ -n "${TARGET}" ] || { echo "${PROG}: --target is required for 'arm-boot'" >&2; usage 1; }
    require_cmd efibootmgr readlink lsblk blkid grep sed

    local base
    base=$(resolve_target "${TARGET}")
    describe "${base}"

    # Find the UEFI boot entry for the target disk. efibootmgr reports entries by
    # label/partition; the operator confirms the match before we arm it.
    echo "==> Current UEFI boot entries:"
    efibootmgr

    echo
    echo "Identify the boot entry number (XXXX) for /dev/${base}'s live media above."
    local entry
    if [ "${ASSUME_YES}" = "true" ]; then
        die "arm-boot needs an interactive boot-entry selection; re-run without --yes"
    fi
    read -r -p "Enter the 4-digit boot entry to set as BootNext (or blank to abort): " entry
    [ -n "${entry}" ] || die "no boot entry selected; aborting"
    [[ "${entry}" =~ ^[0-9A-Fa-f]{4}$ ]] || die "invalid boot entry (expected 4 hex digits): ${entry}"

    # The format check does not prove the entry boots the target media. Confirm
    # the entry's device path resolves to /dev/${base} (by GPT partition UUID or
    # MBR disk signature) before arming it, so arm-boot cannot reboot the host into
    # its own disk, the network, or some other device.
    if ! entry_matches_target "${entry}" "${base}"; then
        die "boot entry ${entry} does not resolve to /dev/${base}; refusing to set BootNext. Verify the entry with 'efibootmgr -v' and that the target media is enumerated."
    fi
    echo "==> Verified boot entry ${entry} points at /dev/${base}"

    local arm_cmd="efibootmgr --bootnext ${entry}"
    if [ "${DRY_RUN}" = "true" ]; then
        echo "==> [dry-run] Would run:"
        echo "    ${arm_cmd}"
        echo "    systemctl reboot"
        return 0
    fi

    if [ "$(id -u)" -ne 0 ]; then
        die "must run as root to set BootNext and reboot"
    fi
    echo "==> Arming one-shot boot into entry ${entry}"
    efibootmgr --bootnext "${entry}"
    echo "==> Rebooting"
    systemctl reboot
}

function parse_common {
    while [ $# -gt 0 ]; do
        case "$1" in
            --image)
                IMAGE="${2:?--image requires a value}"; shift 2 ;;
            --target)
                TARGET="${2:?--target requires a value}"; shift 2 ;;
            --dry-run)
                DRY_RUN=true; shift ;;
            --yes)
                ASSUME_YES=true; shift ;;
            -h|--help)
                usage 0 ;;
            *)
                echo "${PROG}: unknown argument: $1" >&2; usage 1 ;;
        esac
    done
}

function main {
    [ $# -ge 1 ] || usage 1
    SUBCMD="$1"
    case "${SUBCMD}" in
        -h|--help)
            usage 0 ;;
        write)
            shift; parse_common "$@"; cmd_write ;;
        arm-boot|reboot)
            shift; parse_common "$@"; cmd_arm_boot ;;
        *)
            echo "${PROG}: unknown sub-command: ${SUBCMD}" >&2; usage 1 ;;
    esac
}

main "$@"
