#!/bin/bash
#
# usb-ibi-assemble-media.sh - throwaway bootstrap tooling for USB-IBI testing (CNF-27197).
#
# Hand-assembles the initial four-partition USB-IBI test media before the
# create-usb stories (CNF-27158 and later) land, so the media can be boot-tested
# on reference GNR-D lab hardware under UEFI Secure Boot. This is a Phase-0 spike
# aid - it is deliberately simple and NOT a replacement for the eventual
# `lca-cli create-usb` command.
#
# It implements candidate layout (a) - "append after the isohybrid ISO":
# the RHCOS live ISO is written to the front of the image as an isohybrid, then
# the data partitions are appended in the free space after it. See
# docs/enhancements/usb-ibi-testing-workflow.md for the full rationale and the
# un-implemented alternatives (b) and (c).
#
# Partitions are addressed BY FILESYSTEM LABEL, never by a hard-coded GPT number,
# so both media profiles share one code path. GPT partitions are numbered
# contiguously 1..N in on-disk order; the pN names from the proposal are logical
# roles identified by label:
#
#   self-contained : GPT1 live ISO | GPT2 ibi-images | GPT3 cluster-config | GPT4 ibi-status
#   install-only   : GPT1 live ISO | GPT2 ibi-images | GPT3 ibi-status
#
# Requires (run as root, destructive to the output image/device):
#   sgdisk, losetup, partprobe, blkid, mkfs.xfs, mkfs.ext4, cp/dd, mount, sync
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
declare KEEP_LOOP=false

# FS labels - the on-node code (prepare / post-pivot) discovers partitions by
# exactly these strings, so they are the contract and must not change.
declare -r LABEL_IMAGES="ibi-images"
declare -r LABEL_CONFIG="cluster-config"
declare -r LABEL_STATUS="ibi-status"

declare LOOP_DEV=
declare -a MOUNTS=()

function usage {
    cat <<EOF
Usage: ${PROG} --profile <self-contained|install-only> --iso <live.iso> --output <media.img> --ibi-images <oci-dir> [--cluster-config <tree-dir>] [--ignition <config.ign>] [--images-size <MB>] [--config-size <MB>] [--status-size <MB>]

Assembles the initial USB-IBI test media (layout "a": append after the ISO).

Options:
    --profile         Media profile. "self-contained" builds the ISO plus the
                      ibi-images, cluster-config and ibi-status partitions;
                      "install-only" omits cluster-config. (required)
    --iso             RHCOS live ISO written to the front of the media. (required)
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
output image/device and must run as root.
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

# embed_ignition optionally embeds the ignition config into the ISO so the live
# environment auto-runs 'lca-cli ibi -f <cfg>'.
function embed_ignition {
    [ -n "${IGNITION}" ] || return 0
    require_cmd coreos-installer
    echo "==> Embedding ignition config into ${ISO}"
    coreos-installer iso ignition embed --force --ignition-file "${IGNITION}" "${ISO}"
}

# create_image sizes and lays down the output image, writes the ISO to the front
# as an isohybrid, then attaches it to a loop device.
function create_image {
    local iso_bytes total_mb iso_mb
    iso_bytes=$(stat -c %s "${ISO}")
    iso_mb=$(( (iso_bytes + 1048575) / 1048576 ))

    total_mb=$(( iso_mb + IMAGES_SIZE_MB + STATUS_SIZE_MB + SLACK_MB ))
    if [ "${PROFILE}" = "self-contained" ]; then
        total_mb=$(( total_mb + CONFIG_SIZE_MB ))
    fi

    echo "==> Creating ${OUTPUT} (${total_mb} MB; ISO ${iso_mb} MB + data partitions)"
    # Step 1: write the isohybrid live ISO to the front of the output, then grow
    # the file to the full media size so the data partitions have room to follow.
    dd if="${ISO}" of="${OUTPUT}" bs=1M conv=fsync status=none
    truncate -s "${total_mb}M" "${OUTPUT}"

    LOOP_DEV=$(losetup --find --show --partscan "${OUTPUT}")
    echo "==> Attached ${OUTPUT} to ${LOOP_DEV}"
}

# append_partitions converts the ISO's table to GPT, moves the backup header to
# the end of the grown media, and appends the data partitions in free space.
function append_partitions {
    echo "==> Repairing GPT and moving backup header to end of device"
    # Step 2: the ISO wrote a protective MBR / small table at the front; relocate
    # the secondary GPT header to the end of the now-larger device.
    sgdisk --move-second-header "${LOOP_DEV}"

    echo "==> Appending data partitions (first-free-sector, sized)"
    # Step 3: append each data partition into the free space after the ISO, in
    # label order. partition number 0 means "next free number".
    sgdisk --new=0:0:+"${IMAGES_SIZE_MB}"M \
        --change-name=0:"${LABEL_IMAGES}" "${LOOP_DEV}"
    if [ "${PROFILE}" = "self-contained" ]; then
        sgdisk --new=0:0:+"${CONFIG_SIZE_MB}"M \
            --change-name=0:"${LABEL_CONFIG}" "${LOOP_DEV}"
    fi
    sgdisk --new=0:0:+"${STATUS_SIZE_MB}"M \
        --change-name=0:"${LABEL_STATUS}" "${LOOP_DEV}"

    partprobe "${LOOP_DEV}"
    udevadm settle 2>/dev/null || true
}

# part_dev_by_gpt_label prints the partition device node whose GPT partition name
# matches the given label (set with --change-name above). We resolve by GPT name
# rather than by a hard-coded number: the live ISO itself contributes a VARIABLE
# number of GPT partitions (ISO9660 filesystem + EFI System Partition), so the
# appended data partitions are not at fixed indices. Addressing anything by a
# fixed number here risks running mkfs over the ISO's ESP and breaking boot.
function part_dev_by_gpt_label {
    local label="$1"
    local name
    name=$(lsblk -rno NAME,PARTLABEL "${LOOP_DEV}" \
        | awk -v l="${label}" '$2 == l { print $1; exit }')
    [ -n "${name}" ] || die "could not resolve partition by GPT label: ${label}"
    echo "/dev/${name}"
}

# format_and_label creates the filesystem on each data partition and sets its FS
# label to the exact contract string. The label - not the GPT number - is how
# the on-node code finds each partition, so this step is mandatory. Partitions
# are located by their GPT name (set during append), never by a fixed index.
function format_and_label {
    echo "==> Formatting and labelling data partitions"
    mkfs.xfs -f -L "${LABEL_IMAGES}" "$(part_dev_by_gpt_label "${LABEL_IMAGES}")"
    if [ "${PROFILE}" = "self-contained" ]; then
        mkfs.ext4 -F -L "${LABEL_CONFIG}" "$(part_dev_by_gpt_label "${LABEL_CONFIG}")"
    fi
    mkfs.ext4 -F -L "${LABEL_STATUS}" "$(part_dev_by_gpt_label "${LABEL_STATUS}")"

    partprobe "${LOOP_DEV}"
    udevadm settle 2>/dev/null || true
}

# mount_by_label mounts the partition with the given label under a fresh temp
# dir and records it for cleanup. It sets MOUNT_PATH (a global in the parent
# shell) to the mount point instead of printing it: it MUST NOT be called via
# command substitution, because that runs in a subshell where the MOUNTS update
# would never reach cleanup, leaking the mounts and the loop device on exit.
#
# The partition is resolved on LOOP_DEV by its GPT name (not with a global
# 'blkid -L', which could match a same-labelled partition on another attached
# disk and make populate write onto the wrong media), and its filesystem label
# is confirmed before mounting. Addressing by label (not number) keeps the two
# profiles on one code path.
declare MOUNT_PATH=
function mount_by_label {
    local label="$1"
    local dev fslabel
    dev=$(part_dev_by_gpt_label "${label}")
    fslabel=$(blkid -s LABEL -o value "${dev}" 2>/dev/null || true)
    [ "${fslabel}" = "${label}" ] || \
        die "filesystem label mismatch on ${dev}: expected '${label}', found '${fslabel:-<none>}'"
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

# verify asserts the partition table is consistent and every expected label
# resolves, so a failed assembly fails loudly rather than producing bad media.
function verify {
    echo "==> Verifying partition table and labels"
    sgdisk --verify "${LOOP_DEV}"

    local expected=("${LABEL_IMAGES}" "${LABEL_STATUS}")
    if [ "${PROFILE}" = "self-contained" ]; then
        expected=("${LABEL_IMAGES}" "${LABEL_CONFIG}" "${LABEL_STATUS}")
    fi
    # Resolve each label on LOOP_DEV by its GPT name and confirm the filesystem
    # label there, rather than with a global 'blkid -L' that could match a
    # same-labelled partition on another attached disk and report a false "ok".
    local label dev fslabel
    for label in "${expected[@]}"; do
        dev=$(part_dev_by_gpt_label "${label}")
        fslabel=$(blkid -s LABEL -o value "${dev}" 2>/dev/null || true)
        [ "${fslabel}" = "${label}" ] || \
            die "expected label did not resolve on ${LOOP_DEV} after assembly: ${label} (found '${fslabel:-<none>}' on ${dev})"
        echo "    ok: ${label} -> ${dev}"
    done
}

function main {
    parse_args "$@"
    validate_args
    require_cmd sgdisk losetup partprobe blkid lsblk awk mkfs.xfs mkfs.ext4 mount umount dd truncate stat mktemp

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
