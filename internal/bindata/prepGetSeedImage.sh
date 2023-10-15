#!/bin/bash
#
# Image Based Upgrade Prep - Pull seed image
#

declare SEED_IMAGE=
declare PROGRESS_FILE=

function usage {
    cat <<ENDUSAGE
Parameters:
    --seed-image <image>
    --progress-file <file>
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

LONGOPTS="seed-image:,progress-file:"
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

# Collect / validate information? Verify required files exist?

set_progress "completed-seed-image-pull"

log_it "Pulled seed image"

exit 0
