#!/usr/bin/env bash
set -euoE pipefail ## -E option will cause functions to inherit trap
set -x ## Be more verbose

echo "Reconfiguring single node OpenShift"

# remove this part
mkdir -p /opt/openshift
cd /opt/openshift

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

CONFIG_PATH=/opt/openshift/cluster-configuration
NETWORK_CONFIG_PATH=/opt/openshift/network-configuration

echo "Waiting for ${CONFIG_PATH}"
while [[ ! $(lsblk -f --json | jq -r '.blockdevices[] | select(.label == "relocation-config") | .name') && ! -d "${CONFIG_PATH}" ]]; do
    echo "Waiting for site-config"
    sleep 5
done

DEVICE=$(lsblk -f --json | jq -r '.blockdevices[] | select(.label == "relocation-config") | .name')
if [[ -n "${DEVICE}" && ! -d "${CONFIG_PATH}" ]]; then
    mount_config "${DEVICE}"
    cp -r /mnt/config/* ${CONFIG_PATH}
fi

if [ ! -d "${CONFIG_PATH}" ]; then
    echo "Failed to find cluster configuration at ${CONFIG_PATH}"
    exit 1
fi

echo "${CONFIG_PATH} has been created"
# Replace this with a function that loads values from yaml file
set +o allexport

echo "Network configuration exist"
if [[ -d "${NETWORK_CONFIG_PATH}"/system-connections ]]; then
   # TODO: we might need to delete the connection first
    rm -f /etc/NetworkManager/system-connections/*.nmconnection
    cp "${NETWORK_CONFIG_PATH}"/system-connections/*.nmconnection /etc/NetworkManager/system-connections/ -f
    find /etc/NetworkManager/system-connections/*.nmconnection -type f -exec chmod 600 {} \;
fi
if [[ -f "${NETWORK_CONFIG_PATH}"/hostname ]]; then
    cp "${NETWORK_CONFIG_PATH}"/hostname /etc/hostname
fi

if [[ -f "${NETWORK_CONFIG_PATH}"/primary-ip ]]; then
    cp "${NETWORK_CONFIG_PATH}"/primary-ip /etc/default/node-ip
fi

# TODO: change after adding ip to manifest.json
cp "${NETWORK_CONFIG_PATH}"/primary-ip /etc/default/node-ip

RELOCATION_CONFIG_PATH=/opt/openshift/cluster-configuration
NEW_CLUSTER_NAME=$(jq -r '.cluster_name' "${RELOCATION_CONFIG_PATH}"/clusterinfo/manifest.json)
NEW_BASE_DOMAIN=$(jq -r '.domain' "${RELOCATION_CONFIG_PATH}"/clusterinfo/manifest.json)
NEW_HOST_IP=$(cat /etc/default/node-ip)

cat << EOF > /etc/default/sno_dnsmasq_configuration_overrides
SNO_CLUSTER_NAME_OVERRIDE=${NEW_CLUSTER_NAME}
SNO_BASE_DOMAIN_OVERRIDE=${NEW_BASE_DOMAIN}
SNO_DNSMASQ_IP_OVERRIDE=${NEW_HOST_IP}
EOF

systemctl restart NetworkManager
systemctl disable prepare-installation-configuration.service

if [[ -n "${DEVICE}" ]]; then
    umount_config "${DEVICE}"
fi
