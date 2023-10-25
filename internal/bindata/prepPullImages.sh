#!/bin/bash
#
# Image Based Upgrade Prep - Precache images
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

function build_catalog_regex {
    if grep -q . "${img_mnt}/catalogimages.list"; then
        awk -F: '{print $1 ":"; print $1 "@";}' "${img_mnt}/catalogimages.list" | paste -sd\|
    fi
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

set_progress "started-precache"

# Image should already be pulled and mounted
img_mnt=$(podman image mount --format json | jq -r --arg img "${SEED_IMAGE}" '.[] | select(.Repositories[0] == $img) | .mountpoint')
if [ -z "${img_mnt}" ]; then
    fatal "Seed image is not mounted: ${SEED_IMAGE}"
fi

log_it "Precaching non-catalog images"
grep -vE "$(build_catalog_regex)" "${img_mnt}/containers.list" | xargs --no-run-if-empty --max-args 1 --max-procs 10 crictl pull

log_it "Precaching catalog images"
if grep -q . "${img_mnt}/catalogimages.list"; then
    xargs --no-run-if-empty --max-args 1 --max-procs 10 crictl pull < "${img_mnt}/catalogimages.list"
fi

set_progress "completed-precache"

exit 0
