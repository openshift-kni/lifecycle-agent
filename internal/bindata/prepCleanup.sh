#!/bin/bash
#
# Image Based Upgrade Prep Stage Cleanup
#

declare SEED_IMAGE=

function usage {
    cat <<ENDUSAGE
Parameters:
    --seed-image <image>
ENDUSAGE
    exit 1
}

function cleanup {
    if [ -n "${SEED_IMAGE}" ]; then
        if podman image exists "${SEED_IMAGE}"; then
            podman image unmount "${SEED_IMAGE}"
            podman rmi "${SEED_IMAGE}"
        fi
    fi
}

LONGOPTS="seed-image:"
OPTS=$(getopt -o h --long "${LONGOPTS}" --name "$0" -- "$@")

eval set -- "${OPTS}"

while :; do
    case "$1" in
        --seed-image)
            SEED_IMAGE=$2
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

cleanup

