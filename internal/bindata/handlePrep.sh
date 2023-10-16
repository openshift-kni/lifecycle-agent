#!/bin/bash

function usage {
    cat <<ENDUSAGE
usage text
ENDUSAGE
    exit 1
}

LONGOPTS="seed-image:,progress-file:"
OPTS=$(getopt -o h --long "${LONGOPTS}" --name "$0" -- "$@")

eval set -- "${OPTS}"

while :; do
    case "$1" in
        --seed-image)
            # shellcheck disable=SC2034
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

echo "Starting" > $PROGRESS_FILE
sleep 60
echo "Completed" > $PROGRESS_FILE
