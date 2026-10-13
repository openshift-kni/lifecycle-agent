#!/bin/bash
#
# usb-ibi-assemble-media.sh - throwaway bootstrap tooling for USB-IBI testing (CNF-27197).
#
# Hand-assembles the initial USB-IBI test media before the create-usb stories
# (CNF-27158 and later) land, so the media can be boot-tested on reference GNR-D
# lab hardware under UEFI Secure Boot. This is a Phase-0 spike aid - it is
# deliberately simple and NOT a replacement for the eventual `lca-cli create-usb`
# command.
#
# It implements candidate layout (a) - "append after the live ISO", using the
# ISO's own DOS/MBR table: the RHCOS live ISO is copied unchanged to the front of
# the image, then the data partitions are appended in the free MBR slots after it.
#
# WHY MBR, not GPT (fontivan / Developer of Record, milestone-2 hardware test):
# a real RHCOS isohybrid live ISO has a hybrid DOS/GPT layout, and
# 'sgdisk --move-second-header' fails on it with "Invalid partition data!", so a
# GPT append path cannot assemble media. Appending into the ISO's existing DOS/MBR
# table with sfdisk preserved the ISO and its EFI entries; Linux exposed the added
# partitions and their XFS/ext4 labels resolved (validated via loop-device
# inspection; not yet booted on hardware - see the findings doc).
#
# Partitions are addressed BY FILESYSTEM LABEL, never by a hard-coded partition
# number, so both media profiles share one discovery path. They are CREATED in a
# fixed order in the free MBR slots after the ISO:
#
#   install-only   : <ISO primaries> | p3 ibi-images | p4 ibi-status
#   self-contained : <ISO primaries> | p3 ibi-images | p4 extended
#                                       -> p5 cluster-config | p6 ibi-status
#
# The ISO's own primary partitions are snapshotted before the append and are never
# touched, so mkfs can never clobber the ISO's ISO9660 / EFI partitions.
#
# Requires (run as root, destructive to the output image/device):
#   sfdisk, losetup, blockdev, udevadm, blkid, lsblk, awk, mkfs.xfs, mkfs.ext4,
#   dd, truncate, stat, mktemp, mount, umount, sync
#   coreos-installer (only when --ignition is given, to embed the ignition config)
#

set -euo pipefail

PROG=$(basename "$0")

declare PROFILE=
declare ISO=
declare OUTPUT=
declare IBI_IMAGES_SRC=
declare CLUSTER_CONFIG_SRC=
declare IGNITION=
declare -i IMAGES_SIZE_MB=4096
declare -i CONFIG_SIZE_MB=512
declare -i STATUS_SIZE_MB=256
declare -i SLACK_MB=64
# EXT_SLACK_MB pads the self-contained extended container so it holds both logical
# partitions plus their per-logical EBR + 1 MiB alignment overhead.
declare -i EXT_SLACK_MB=16
declare KEEP_LOOP=false

# FS labels - the on-node code (prepare / post-pivot) discovers partitions by
# exactly these strings, so they are the contract and must not change.
declare -r LABEL_IMAGES="ibi-images"
declare -r LABEL_CONFIG="cluster-config"
declare -r LABEL_STATUS="ibi-status"

declare LOOP_DEV=
declare -a MOUNTS=()
# ISO_PARTS holds the partition device nodes that belong to the live ISO (captured
# before the data partitions are appended). They are never formatted or mounted.
declare -a ISO_PARTS=()
# EMBEDDED_ISO_DIR holds the temp copy of the ISO that embed_ignition writes to so
# the operator's source ISO is never modified in place; cleanup removes it.
declare EMBEDDED_ISO_DIR=

function usage {
    cat <<EOF
Usage: ${PROG} --profile <self-contained|install-only> --iso <live.iso> --output <media.img> --ibi-images <oci-dir> [--cluster-config <tree-dir>] [--ignition <config.ign>] [--images-size <MB>] [--config-size <MB>] [--status-size <MB>]

Assembles the initial USB-IBI test media (layout "a": append after the ISO, MBR).

Options:
    --profile         Media profile. "self-contained" builds the ISO plus the
                      ibi-images, cluster-config and ibi-status partitions;
                      "install-only" omits cluster-config. (required)
    --iso             RHCOS live ISO copied unchanged to the front of the media. (required)
    --output          Output image (.img) file to assemble. (required)
    --ibi-images      Directory populated into the ${LABEL_IMAGES} partition
                      (an OCI image layout, e.g. from 'skopeo copy oci:...' or
                      oc-mirror output). (required)
    --cluster-config  Directory populated into the ${LABEL_CONFIG} partition
                      (the /opt/openshift/... reconfigure tree).
                      (required for self-contained, rejected for install-only)
    --ignition        Optional ignition config embedded into the ISO with
                      'coreos-installer iso ignition embed' so the live
                      environment auto-runs 'lca-cli ibi -f <cfg>'.
    --images-size     Size of the ${LABEL_IMAGES} partition in MB (default ${IMAGES_SIZE_MB}).
    --config-size     Size of the ${LABEL_CONFIG} partition in MB (default ${CONFIG_SIZE_MB}).
    --status-size     Size of the ${LABEL_STATUS} partition in MB (default ${STATUS_SIZE_MB}).
    --keep-loop       Do not detach the loop device on exit (debugging).
    -h, --help        Show this help and exit.

Example:
    sudo ${PROG} --profile self-contained --iso rhcos-live.iso --output usb-ibi.img --ibi-images ./oci-layout --cluster-config ./opt-openshift --ignition ./ibi.ign

Note: this is throwaway spike tooling (CNF-27197). It is destructive to the
output image/device and must run as root. It operates entirely on the output
image file; write it to a stick or attach it as virtual media separately.
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

# cleanup unmounts everything we mounted and detaches the loop device.
function cleanup {
    local rc=$?
    local m
    # Unmount every temp mount we created (each is an independent mktemp dir, so
    # order does not matter).
    if [ "${#MOUNTS[@]}" -gt 0 ]; then
        for m in "${MOUNTS[@]}"; do
            if mountpoint -q "${m}" 2>/dev/null; then
                umount "${m}" 2>/dev/null || umount -l "${m}" 2>/dev/null || true
            fi
            rmdir "${m}" 2>/dev/null || true
        done
    fi
    if [ -n "${LOOP_DEV}" ] && [ "${KEEP_LOOP}" != "true" ]; then
        losetup -d "${LOOP_DEV}" 2>/dev/null || true
    fi
    if [ -n "${EMBEDDED_ISO_DIR}" ]; then
        rm -rf -- "${EMBEDDED_ISO_DIR}" 2>/dev/null || true
    fi
    return "${rc}"
}

function parse_args {
    while [ $# -gt 0 ]; do
        case "$1" in
            --profile)
                PROFILE="${2:?--profile requires a value}"; shift 2 ;;
            --iso)
                ISO="${2:?--iso requires a value}"; shift 2 ;;
            --output)
                OUTPUT="${2:?--output requires a value}"; shift 2 ;;
            --ibi-images)
                IBI_IMAGES_SRC="${2:?--ibi-images requires a value}"; shift 2 ;;
            --cluster-config)
                CLUSTER_CONFIG_SRC="${2:?--cluster-config requires a value}"; shift 2 ;;
            --ignition)
                IGNITION="${2:?--ignition requires a value}"; shift 2 ;;
            --images-size)
                IMAGES_SIZE_MB="${2:?--images-size requires a value}"; shift 2 ;;
            --config-size)
                CONFIG_SIZE_MB="${2:?--config-size requires a value}"; shift 2 ;;
            --status-size)
                STATUS_SIZE_MB="${2:?--status-size requires a value}"; shift 2 ;;
            --keep-loop)
                KEEP_LOOP=true; shift ;;
            -h|--help)
                usage 0 ;;
            *)
                echo "${PROG}: unknown argument: $1" >&2; usage 1 ;;
        esac
    done
}

function validate_args {
    [ -n "${PROFILE}" ] || { echo "${PROG}: --profile is required" >&2; usage 1; }
    [ -n "${ISO}" ] || { echo "${PROG}: --iso is required" >&2; usage 1; }
    [ -n "${OUTPUT}" ] || { echo "${PROG}: --output is required" >&2; usage 1; }
    [ -n "${IBI_IMAGES_SRC}" ] || { echo "${PROG}: --ibi-images is required" >&2; usage 1; }

    case "${PROFILE}" in
        self-contained)
            [ -n "${CLUSTER_CONFIG_SRC}" ] || \
                die "--cluster-config is required for the self-contained profile"
            ;;
        install-only)
            [ -z "${CLUSTER_CONFIG_SRC}" ] || \
                die "--cluster-config is not valid for the install-only profile (no cluster-config partition)"
            ;;
        *)
            die "unknown --profile '${PROFILE}' (expected self-contained or install-only)"
            ;;
    esac

    [ -f "${ISO}" ] || die "ISO not found: ${ISO}"
    # create_image runs 'dd if=${ISO} of=${OUTPUT}', which truncates the output
    # before reading the source. If --output aliases --iso the ISO is destroyed
    # and the padded image is invalid, so reject the alias while still allowing a
    # distinct existing output to be replaced. (CodeRabbit review)
    if [ -e "${OUTPUT}" ] && [ "${ISO}" -ef "${OUTPUT}" ]; then
        die "--output must not refer to the same file as --iso: ${OUTPUT}"
    fi
    [ -d "${IBI_IMAGES_SRC}" ] || die "--ibi-images directory not found: ${IBI_IMAGES_SRC}"
    if [ -n "${CLUSTER_CONFIG_SRC}" ]; then
        [ -d "${CLUSTER_CONFIG_SRC}" ] || die "--cluster-config directory not found: ${CLUSTER_CONFIG_SRC}"
    fi
    if [ -n "${IGNITION}" ]; then
        [ -f "${IGNITION}" ] || die "--ignition file not found: ${IGNITION}"
    fi
    for size in "${IMAGES_SIZE_MB}" "${CONFIG_SIZE_MB}" "${STATUS_SIZE_MB}"; do
        [ "${size}" -gt 0 ] || die "partition sizes must be positive"
    done

    if [ "$(id -u)" -ne 0 ]; then
        die "must run as root (partitioning and mounting require privileges)"
    fi
}

# embed_ignition optionally embeds the ignition config so the live environment
# auto-runs 'lca-cli ibi -f <cfg>'. 'coreos-installer iso ignition embed' rewrites
# the positional ISO in place, so we embed into a throwaway copy and repoint ISO at
# it; the operator's source ISO is left untouched (create_image then copies the
# embedded copy). cleanup removes the temp dir.
function embed_ignition {
    [ -n "${IGNITION}" ] || return 0
    require_cmd coreos-installer mktemp
    local embedded_iso
    EMBEDDED_ISO_DIR=$(mktemp -d)
    embedded_iso="${EMBEDDED_ISO_DIR}/$(basename "${ISO}")"
    echo "==> Embedding ignition config into a temporary copy of ${ISO}"
    coreos-installer iso ignition embed --force --ignition-file "${IGNITION}" \
        --output "${embedded_iso}" "${ISO}"
    ISO="${embedded_iso}"
}

# create_image sizes and lays down the output image, copies the live ISO unchanged
# to the front, then attaches it to a loop device. The ISO is copied byte-for-byte
# so its DOS/MBR table (and the EFI entries that make it bootable) are preserved.
function create_image {
    local iso_bytes total_mb iso_mb
    iso_bytes=$(stat -c %s "${ISO}")
    iso_mb=$(( (iso_bytes + 1048575) / 1048576 ))

    total_mb=$(( iso_mb + IMAGES_SIZE_MB + STATUS_SIZE_MB + SLACK_MB ))
    if [ "${PROFILE}" = "self-contained" ]; then
        total_mb=$(( total_mb + CONFIG_SIZE_MB + EXT_SLACK_MB ))
    fi

    echo "==> Creating ${OUTPUT} (${total_mb} MB; ISO ${iso_mb} MB + data partitions)"
    # Step 1: copy the live ISO unchanged to the front of the output, then grow the
    # file to the full media size so the data partitions have room to follow.
    dd if="${ISO}" of="${OUTPUT}" bs=1M conv=fsync status=none
    truncate -s "${total_mb}M" "${OUTPUT}"

    LOOP_DEV=$(losetup --find --show --partscan "${OUTPUT}")
    echo "==> Attached ${OUTPUT} to ${LOOP_DEV}"
}

# reread_table asks the kernel to re-read LOOP_DEV's partition table and waits for
# the device nodes to settle. partprobe is NOT used: it is absent on the RHCOS
# base OS (fontivan / DoR), so the supported reread is 'blockdev --rereadpt'. A
# failed reread means the kernel may still expose the old layout, so it aborts.
function reread_table {
    blockdev --rereadpt "${LOOP_DEV}" || die "could not reread partition table on ${LOOP_DEV}"
    udevadm settle 2>/dev/null || true
}

# iso_part_nodes prints every partition device node currently in LOOP_DEV's table.
function iso_part_nodes {
    sfdisk --dump "${LOOP_DEV}" 2>/dev/null | awk '/ : start=/ { print $1 }'
}

# is_iso_part returns success if the given device node is one of the ISO's own
# partitions snapshotted before the append (and therefore must never be touched).
function is_iso_part {
    local d="$1" p
    for p in "${ISO_PARTS[@]}"; do
        [ "${p}" = "${d}" ] && return 0
    done
    return 1
}

# data_part_devs prints the data partition device nodes we appended, in creation
# order (ibi-images, [cluster-config,] ibi-status). It selects the Linux (type 83)
# partitions from the current table and excludes both the ISO's own partitions and
# the extended container (type 5), so the mapping never depends on a fixed index
# and can never pick an ISO partition.
function data_part_devs {
    local dev
    while read -r dev; do
        is_iso_part "${dev}" && continue
        echo "${dev}"
    done < <(sfdisk --dump "${LOOP_DEV}" 2>/dev/null | awk '/ : start=/ && /type=83/ { print $1 }')
}

# append_partitions appends the data partitions into the free MBR slots after the
# ISO using sfdisk. The ISO's hybrid DOS/GPT layout is left intact (no GPT backup
# relocation - it fails on the real ISO); sfdisk --append only adds partitions.
function append_partitions {
    echo "==> Snapshotting the ISO's partitions (never touched) before appending"
    mapfile -t ISO_PARTS < <(iso_part_nodes)
    [ "${#ISO_PARTS[@]}" -gt 0 ] || \
        die "no partitions found on ${LOOP_DEV}; is ${ISO} a RHCOS isohybrid live ISO?"

    echo "==> Appending data partitions into the free MBR slots after the ISO"
    if [ "${PROFILE}" = "self-contained" ]; then
        # p3 = ibi-images (primary), p4 = extended container, p5 = cluster-config
        # (logical), p6 = ibi-status (logical). The extended is sized to hold both
        # logicals plus their EBR/alignment overhead (EXT_SLACK_MB).
        local -i ext_mb
        ext_mb=$(( CONFIG_SIZE_MB + STATUS_SIZE_MB + EXT_SLACK_MB ))
        sfdisk --append "${LOOP_DEV}" <<EOF
size=${IMAGES_SIZE_MB}M, type=83
size=${ext_mb}M, type=5
size=${CONFIG_SIZE_MB}M, type=83
size=${STATUS_SIZE_MB}M, type=83
EOF
    else
        # install-only: the two free primary slots p3 = ibi-images, p4 = ibi-status.
        sfdisk --append "${LOOP_DEV}" <<EOF
size=${IMAGES_SIZE_MB}M, type=83
size=${STATUS_SIZE_MB}M, type=83
EOF
    fi

    reread_table
}

# format_and_label creates the filesystem on each data partition and sets its FS
# label to the exact contract string. The label - not the partition number - is
# how the on-node code finds each partition, so this step is mandatory. The
# partitions are addressed in creation order from data_part_devs (which already
# excludes the ISO's partitions and the extended container).
function format_and_label {
    echo "==> Formatting and labelling data partitions"
    local -a parts
    mapfile -t parts < <(data_part_devs)

    local -i want=2
    [ "${PROFILE}" = "self-contained" ] && want=3
    [ "${#parts[@]}" -eq "${want}" ] || \
        die "expected ${want} appended data partitions, found ${#parts[@]}: ${parts[*]:-<none>}"

    mkfs.xfs -f -L "${LABEL_IMAGES}" "${parts[0]}"
    if [ "${PROFILE}" = "self-contained" ]; then
        mkfs.ext4 -F -L "${LABEL_CONFIG}" "${parts[1]}"
        mkfs.ext4 -F -L "${LABEL_STATUS}" "${parts[2]}"
    else
        mkfs.ext4 -F -L "${LABEL_STATUS}" "${parts[1]}"
    fi

    udevadm settle 2>/dev/null || true
}

# part_dev_by_fs_label prints the partition device node on LOOP_DEV whose
# FILESYSTEM label matches the given label. It scans only LOOP_DEV's own
# partitions (not a global 'blkid -L', which could match a same-labelled partition
# on another attached disk and make populate write onto the wrong media). MBR
# partitions carry no partition name, so the filesystem label set by
# format_and_label is the only reliable, profile-independent key.
function part_dev_by_fs_label {
    local label="$1"
    local dev fslabel
    while read -r dev; do
        [ -b "${dev}" ] || continue
        fslabel=$(blkid -s LABEL -o value "${dev}" 2>/dev/null || true)
        if [ "${fslabel}" = "${label}" ]; then
            echo "${dev}"
            return 0
        fi
    done < <(lsblk -rno NAME,TYPE "${LOOP_DEV}" | awk '$2 == "part" { print "/dev/"$1 }')
    die "could not resolve partition by filesystem label on ${LOOP_DEV}: ${label}"
}

# mount_by_label mounts the partition with the given filesystem label under a fresh
# temp dir and records it for cleanup. It sets MOUNT_PATH (a global in the parent
# shell) to the mount point instead of printing it: it MUST NOT be called via
# command substitution, because that runs in a subshell where the MOUNTS update
# would never reach cleanup, leaking the mounts and the loop device on exit.
#
# The partition is resolved on LOOP_DEV by its filesystem label (see
# part_dev_by_fs_label), which also confirms the label before we mount it.
declare MOUNT_PATH=
function mount_by_label {
    local label="$1"
    local dev
    dev=$(part_dev_by_fs_label "${label}")
    MOUNT_PATH=$(mktemp -d)
    mount "${dev}" "${MOUNT_PATH}"
    MOUNTS+=("${MOUNT_PATH}")
}

# populate copies the source payloads into each partition, addressing them by
# label, then syncs. ibi-status is intentionally left empty.
function populate {
    echo "==> Populating partitions (addressed by label)"
    # mount_by_label sets MOUNT_PATH in this (parent) shell; it must not be called
    # via command substitution or the MOUNTS registration would be lost.
    mount_by_label "${LABEL_IMAGES}"
    echo "    ${LABEL_IMAGES} <- ${IBI_IMAGES_SRC}"
    cp -a "${IBI_IMAGES_SRC}/." "${MOUNT_PATH}/"

    if [ "${PROFILE}" = "self-contained" ]; then
        mount_by_label "${LABEL_CONFIG}"
        echo "    ${LABEL_CONFIG} <- ${CLUSTER_CONFIG_SRC}"
        cp -a "${CLUSTER_CONFIG_SRC}/." "${MOUNT_PATH}/"
    fi

    # ibi-status is left empty (the prepare/reconfigure flow writes it on-node);
    # mounting it here only confirms the filesystem is writable.
    mount_by_label "${LABEL_STATUS}"
    echo "    ${LABEL_STATUS} <- (left empty, writable)"

    sync
}

# verify_mbr_table runs 'sfdisk --verify' but tolerates the one overlap that is
# inherent to a RHCOS isohybrid ISO: its partition 1 (the whole ISO9660 image)
# deliberately contains partition 2 (the ESP), so sfdisk always reports
# "Partition 1: overlaps partition 2" for the untouched ISO entries. sfdisk then
# counts that overlap in a trailing "N error(s) detected." summary and exits
# non-zero. Both the overlap line and the summary it produces are expected and
# harmless - the data partitions we appended sit after the ISO and overlap
# nothing - so we strip both before judging the rest, and fail only if sfdisk
# reports (or counts) anything beyond the expected overlap. (fontivan /
# Developer of Record)
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
        die "partition table verification failed on ${dev} (unexpected overlap)"
    fi

    # Every error sfdisk counted must be one of the expected ISO overlaps; any
    # extra error - or a non-zero exit we cannot attribute to the overlap - is fatal.
    if [ "${detected}" -gt "${expected_overlaps}" ]; then
        printf '%s\n' "${out}" >&2
        die "partition table verification failed on ${dev} (${detected} error(s) detected, ${expected_overlaps} expected)"
    fi
    if [ "${rc}" -ne 0 ] && [ "${detected}" -eq 0 ] && [ "${expected_overlaps}" -eq 0 ]; then
        printf '%s\n' "${out}" >&2
        die "partition table verification failed on ${dev} (sfdisk exit ${rc})"
    fi

    if [ "${expected_overlaps}" -gt 0 ]; then
        echo "    ok: partition table verified (ISO's expected p1/p2 hybrid overlap ignored)"
    else
        echo "    ok: partition table verified"
    fi
}

# verify asserts the partition table is consistent and every expected label
# resolves, so a failed assembly fails loudly rather than producing bad media.
function verify {
    echo "==> Verifying partition table and filesystem labels"
    verify_mbr_table "${LOOP_DEV}"

    local expected=("${LABEL_IMAGES}" "${LABEL_STATUS}")
    if [ "${PROFILE}" = "self-contained" ]; then
        expected=("${LABEL_IMAGES}" "${LABEL_CONFIG}" "${LABEL_STATUS}")
    fi
    # Resolve each label on LOOP_DEV by its filesystem label and confirm it, rather
    # than with a global 'blkid -L' that could match a same-labelled partition on
    # another attached disk and report a false "ok".
    local label dev
    for label in "${expected[@]}"; do
        dev=$(part_dev_by_fs_label "${label}")
        echo "    ok: ${label} -> ${dev}"
    done
}

function main {
    parse_args "$@"
    validate_args
    require_cmd sfdisk losetup blockdev udevadm blkid lsblk awk mkfs.xfs mkfs.ext4 mount umount dd truncate stat mktemp

    trap cleanup EXIT

    embed_ignition
    create_image
    append_partitions
    format_and_label
    populate
    verify

    echo
    echo "==> Done. Assembled ${PROFILE} media at ${OUTPUT}"
    echo "    Boot-test it on reference GNR-D lab hardware under Secure Boot,"
    echo "    or attach the whole .img as BMC virtual media (see the findings doc)."
}

main "$@"
