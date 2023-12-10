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
if [[ -n "${DEVICE}" && ! -d "${OPT_OPENSHIFT}" ]]; then
    mount_config "${DEVICE}"
    cp -r /mnt/config/* ${OPT_OPENSHIFT}
fi

if [ ! -d "${OPT_OPENSHIFT}" ]; then
    echo "Failed to find cluster configuration at ${CONFIG_PATH}"
    exit 1
fi

echo "${OPT_OPENSHIFT} has been created"
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

CLUSTER_CONFIG_FILE="${CONFIG_PATH}"/manifest.json
NEW_CLUSTER_NAME=$(jq -r '.cluster_name' "${CLUSTER_CONFIG_FILE}")
NEW_BASE_DOMAIN=$(jq -r '.domain' "${CLUSTER_CONFIG_FILE}")
NEW_HOST_IP=$(jq -r '.master_ip' "${CLUSTER_CONFIG_FILE}")

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
