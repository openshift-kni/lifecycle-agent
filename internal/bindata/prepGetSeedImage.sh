#!/bin/bash
#
# Image Based Upgrade Prep - Pull seed image
#

declare SEED_IMAGE=
declare PROGRESS_FILE=
declare IMAGE_LIST_FILE=

function usage {
    cat <<ENDUSAGE
Parameters:
    --seed-image <image>
    --progress-file <file>
    --image-list-file <file>
ENDUSAGE
    exit 1
}

function set_progress {
    echo "$1" > "${PROGRESS_FILE}"
}

function fatal {
    set_progress "Failed"
    echo "$@" >&2
    exit 1
}

function log_it {
    echo "$@" | tr '[:print:]' -
    echo "$@"
    echo "$@" | tr '[:print:]' -
}

function build_catalog_regex {
    if grep -q . "${img_mnt}/catalogimages.list"; then
        awk -F: '{print $1 ":"; print $1 "@";}' "${img_mnt}/catalogimages.list" | paste -sd\|
    fi
}

LONGOPTS="seed-image:,progress-file:,image-list-file:"
OPTS=$(getopt -o h --long "${LONGOPTS}" --name "$0" -- "$@")

eval set -- "${OPTS}"

while :; do
    case "$1" in
        --seed-image)
            SEED_IMAGE=$2
            shift 2
            ;;
        --progress-file)
            PROGRESS_FILE=$2
            shift 2
            ;;
        --image-list-file)
            IMAGE_LIST_FILE=$2
            shift 2
            ;;
        --)
            shift
            break
            ;;
        *)
            usage
            exit 1
            ;;
    esac
done

set_progress "started-seed-image-pull"

log_it "Pulling and mounting seed image"
podman pull "${SEED_IMAGE}" || fatal "Failed to pull image: ${SEED_IMAGE}"

img_mnt=$(podman image mount "${SEED_IMAGE}")
if [ -z "${img_mnt}" ]; then
    fatal "Failed to mount image: ${SEED_IMAGE}"
fi

# Extract list of images to be pre-cached
log_it "Extracting precaching image list of non-catalog images"
grep -vE "$(build_catalog_regex)" "${img_mnt}/containers.list" > "${IMAGE_LIST_FILE}"

log_it "Extracting precaching image list of catalog images"
if grep -q . "${img_mnt}/catalogimages.list"; then
    cat "${img_mnt}/catalogimages.list"  >> "${IMAGE_LIST_FILE}"
fi

log_it "Finished preparing image list for pre-caching"

# Collect / validate information? Verify required files exist?

set_progress "completed-seed-image-pull"

log_it "Pulled seed image"

exit 0
