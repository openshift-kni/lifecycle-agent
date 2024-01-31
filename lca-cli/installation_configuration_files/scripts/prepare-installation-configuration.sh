#!/usr/bin/env bash
set -euoE pipefail ## -E option will cause functions to inherit trap
set -x ## Be more verbose

echo "Reconfiguring single node OpenShift"

# remove this part

OPT_OPENSHIFT=/opt/openshift

mkdir -p ${OPT_OPENSHIFT}
cd ${OPT_OPENSHIFT}

function mount_config {
    echo "Mounting config iso"
    mkdir -p /mnt/config
    if [[ ! $(mountpoint --quiet /mnt/config) ]]; then
        mount "/dev/$1" /mnt/config
    fi
    ls /mnt/config
}

function umount_config {
    echo "Unmounting config iso"
    umount /dev/$1
    rm -rf /mnt/config
}


CONFIG_PATH=${OPT_OPENSHIFT}/cluster-configuration
NETWORK_CONFIG_PATH=${OPT_OPENSHIFT}/network-configuration

echo "Waiting for ${CONFIG_PATH}"
while [[ ! $(lsblk -f --json | jq -r '.blockdevices[] | select(.label == "relocation-config") | .name') && ! -d "${CONFIG_PATH}" ]]; do
    echo "Waiting for site-config"
    sleep 5
done

DEVICE=$(lsblk -f --json | jq -r '.blockdevices[] | select(.label == "relocation-config") | .name')
if [[ -n "${DEVICE}" && ! -d "${CONFIG_PATH}" ]]; then
    mount_config "${DEVICE}"
    cp -r /mnt/config/* ${OPT_OPENSHIFT}
fi

if [ ! -d "${OPT_OPENSHIFT}" ]; then
    echo "Failed to find cluster configuration at ${CONFIG_PATH}"
    exit 1
fi

echo "${OPT_OPENSHIFT} has been created"

set +o allexport

echo "Network configuration exist"
if [[ -d "${NETWORK_CONFIG_PATH}"/system-connections ]]; then
    rm -f /etc/NetworkManager/system-connections/*.nmconnection
    cp "${NETWORK_CONFIG_PATH}"/system-connections/*.nmconnection /etc/NetworkManager/system-connections/ -f
    find /etc/NetworkManager/system-connections/*.nmconnection -type f -exec chmod 600 {} \;
fi

CLUSTER_CONFIG_FILE="${CONFIG_PATH}"/manifest.json
NEW_HOSTNAME=$(jq -r '.hostname' "${CLUSTER_CONFIG_FILE}")
echo ${NEW_HOSTNAME} > /etc/hostname

systemctl restart NetworkManager
systemctl disable prepare-installation-configuration.service

if [[ -n "${DEVICE}" ]]; then
    umount_config "${DEVICE}"
fi
