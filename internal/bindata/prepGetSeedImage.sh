#!/bin/bash
#
# Image Based Upgrade Prep - Pull seed image
#

declare SEED_IMAGE=
declare PROGRESS_FILE=
declare IMAGE_LIST_FILE=
declare PULL_SECRET=

function usage {
    cat <<ENDUSAGE
Parameters:
    --seed-image <image>
    --progress-file <file>
    --image-list-file <file>
    --pull-secret <file>
ENDUSAGE
    exit 1
}

# Since we cannot cleanup easily from prep_handlers.go, we can do it here
function _cleanup {
    if [ -n "${PULL_SECRET}" ] && [ "${PULL_SECRET}" != "/var/lib/kubelet/config.json" ]; then
        rm "${PULL_SECRET}"
    fi
}
trap _cleanup exit

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

LONGOPTS="seed-image:,progress-file:,image-list-file:,pull-secret:"
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
        --pull-secret)
            PULL_SECRET=$2
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
if [ -n "${PULL_SECRET}" ]; then
    pull_secret_arg="--authfile $PULL_SECRET"
fi
podman pull $pull_secret_arg "${SEED_IMAGE}" || fatal "Failed to pull image: ${SEED_IMAGE}"

img_mnt=$(podman image mount "${SEED_IMAGE}")
if [ -z "${img_mnt}" ]; then
    fatal "Failed to mount image: ${SEED_IMAGE}"
fi

# Extract list of images to be pre-cached
log_it "Extracting precaching image list of non-catalog images"
cat "${img_mnt}/containers.list" > "${IMAGE_LIST_FILE}"


log_it "Finished preparing image list for pre-caching"

# Collect / validate information? Verify required files exist?

set_progress "completed-seed-image-pull"

log_it "Pulled seed image"

exit 0
