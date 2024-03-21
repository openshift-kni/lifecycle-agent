#!/bin/bash

set -e # Halt on error

seed_image=${1:-$SEED_IMAGE}
seed_version=${2:-$SEED_VERSION}
installation_disk=${3:-$INSTALLATION_DISK}
lca_image=${4:-$LCA_IMAGE}
extra_partition_start=${5:-$EXTRA_PARTITION_START}
extra_partition_number=5
extra_partition_label=varlibcontainers
create_extra_partition=true

[[ "$extra_partition_start" == "use_directory" ]] && create_extra_partition="false"

authfile=${AUTH_FILE:-"/var/tmp/backup-secret.json"}
pull_secret=${PULL_SECRET_FILE:-"/var/tmp/pull-secret.json"}

coreos-installer install ${installation_disk}

if [[ "$create_extra_partition" == "true" ]]; then
    # Create new partition for /var/lib/containers
    sfdisk ${installation_disk} <<< write
    sgdisk --new $extra_partition_number:$extra_partition_start --change-name $extra_partition_number:$extra_partition_label ${installation_disk}
    mkfs.xfs -f ${installation_disk}$extra_partition_number
fi


# We need to grow the partition. Coreos-installer leaves a small partition
growpart ${installation_disk} 4
mount /dev/disk/by-partlabel/root /mnt
mount /dev/disk/by-partlabel/boot /mnt/boot
xfs_growfs ${installation_disk}4

if [[ "$create_extra_partition" == "true" ]]; then
    # Mount extra partition in /var/lib/containers
    mount /dev/disk/by-partlabel/varlibcontainers /var/lib/containers
else
    # Create and mount directory for /var/lib/containers
    chattr -i /mnt/
    mkdir -p /mnt/containers
    chattr +i /mnt/
    mount -o bind /mnt/containers /var/lib/containers
fi
restorecon -R /var/lib/containers

additional_flags=""
if [ -n "${PRECACHE_DISABLED}" ]; then
    additional_flags="${additional_flags} --precache-disabled"
fi

if [ -n "${PRECACHE_BEST_EFFORT}" ]; then
    additional_flags="${additional_flags} --precache-best-effort"
fi

podman run --privileged --rm --pid=host --authfile "${authfile}" -v /:/host --entrypoint /usr/local/bin/lca-cli "${lca_image}" ibi --seed-image "${seed_image}" --authfile "${authfile}" --seed-version "${seed_version}" --pullSecretFile "${pull_secret}" ${additional_flags}
