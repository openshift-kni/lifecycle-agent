#!/bin/bash

set -e # Halt on error

seed_image=${1:-$SEED_IMAGE}
seed_version=${2:-$SEED_VERSION}
installation_disk=${3:-$INSTALLATION_DISK}
extra_partition_start=${5:-$EXTRA_PARTITION_START}
extra_partition_number=5
extra_partition_label=varlibcontainers

authfile=${AUTH_FILE:-"/var/tmp/backup-secret.json"}
pull_secret=${PULL_SECRET_FILE:-"/var/tmp/pull-secret.json"}


additional_flags=""
if [ -n "${PRECACHE_DISABLED}" ]; then
    additional_flags="${additional_flags} --precache-disabled"
fi

if [ -n "${PRECACHE_BEST_EFFORT}" ]; then
    additional_flags="${additional_flags} --precache-best-effort"
fi

if [ -n "${SHUTDOWN}" ]; then
    additional_flags="${additional_flags} --shutdown"
fi

if [[ ! "$extra_partition_start" == "use_directory" ]]; then
    additional_flags="${additional_flags} --create-extra-partition"
    additional_flags="${additional_flags} --extra-partition-start ${extra_partition_start}"
    additional_flags="${additional_flags} --extra-partition-number ${extra_partition_number}"
    additional_flags="${additional_flags} --extra-partition-label ${extra_partition_label}"
fi

if [ -n "${SKIP_DISK_CLEANUP}" ]; then
    additional_flags="${additional_flags} --skip-disk-cleanup"
fi

podman create --authfile "${authfile}" --name lca-cli "${seed_image}" lca-cli
podman cp lca-cli:lca-cli /usr/local/bin/lca-cli
podman rm lca-cli

/usr/local/bin/lca-cli ibi --seed-image "${seed_image}" --authfile "${authfile}" --seed-version "${seed_version}" --pullSecretFile "${pull_secret}" --installation-disk "${installation_disk}" ${additional_flags}
