#!/bin/bash
#

PROG=$(basename "$0")

declare CLUSTER_NAME=
declare CLUSTER_BASE_DOMAIN=
declare CLUSTER_IP=
declare CLUSTER_IPS=()
declare GENERATE="site-config"
declare -i ZTP_DEPLOY_WAVE=0

function usage {
    cat <<EOF
Usage: ${PROG} --name <cluster> --domain <domain> --ip <addr> [ --mc ] [ --wave <1-100> ]
       ${PROG} --name <cluster> --domain <domain> --ips <addr1,addr2,...> [ --mc ] [ --wave <1-100> ]
Options:
    --name       - Cluster name
    --domain     - Cluster baseDomain
    --ip         - Node IP (for single-stack)
    --ips        - Node IPs comma-separated (for dual-stack, e.g., "192.168.1.10,2001:db8::10")
    --mc         - Generate machine-config (default is site-policy)
    --wave       - Add ztp-deploy-wave annotation with specified value to site-policy

Summary:
    Generates a subsection of site-policy to include dnsmasq config for an SNO.
    Supports both single-stack and dual-stack networking.

Examples:
    Single-stack: ${PROG} --name mysno --domain sno.cluster-domain.com --ip 10.20.30.5
    Dual-stack:   ${PROG} --name mysno --domain sno.cluster-domain.com --ips 10.20.30.5,2001:db8::10
EOF
    exit 1
}

function generate_dnsmasq_content {
    cat <<EOSCRIPT
#!/usr/bin/env bash

# In order to override cluster domain please provide this file with the following params:
# SNO_CLUSTER_NAME_OVERRIDE=<new cluster name>
# SNO_BASE_DOMAIN_OVERRIDE=<your new base domain>
# SNO_DNSMASQ_IP_OVERRIDE=<new ip or comma-separated ips>
source /etc/default/sno_dnsmasq_configuration_overrides

# Parse multiple IPs for dual-stack support
HOST_IP_OVERRIDE=\${SNO_DNSMASQ_IP_OVERRIDE:-"${CLUSTER_IP}"}
CLUSTER_NAME=\${SNO_CLUSTER_NAME_OVERRIDE:-"${CLUSTER_NAME}"}
BASE_DOMAIN=\${SNO_BASE_DOMAIN_OVERRIDE:-"${CLUSTER_BASE_DOMAIN}"}
CLUSTER_FULL_DOMAIN="\${CLUSTER_NAME}.\${BASE_DOMAIN}"

# Convert comma-separated IPs to array
IFS=',' read -ra HOST_IPS <<< "\$HOST_IP_OVERRIDE"

# Generate dnsmasq configuration for all IPs
cat << EOF > /etc/dnsmasq.d/single-node.conf
EOF

for ip in "\${HOST_IPS[@]}"; do
    # Trim whitespace
    ip=\$(echo "\$ip" | xargs)
    if [[ -n "\$ip" ]]; then
        cat << EOF >> /etc/dnsmasq.d/single-node.conf
address=/apps.\${CLUSTER_FULL_DOMAIN}/\$ip
address=/api-int.\${CLUSTER_FULL_DOMAIN}/\$ip
address=/api.\${CLUSTER_FULL_DOMAIN}/\$ip
EOF
    fi
done
EOSCRIPT
}

function generate_forcedns {
    cat <<EOF
#!/bin/bash

# In order to override cluster domain please provide this file with the following params:
# SNO_CLUSTER_NAME_OVERRIDE=<new cluster name>
# SNO_BASE_DOMAIN_OVERRIDE=<your new base domain>
# SNO_DNSMASQ_IP_OVERRIDE=<new ip or comma-separated ips>
source /etc/default/sno_dnsmasq_configuration_overrides

# Parse multiple IPs for dual-stack support
HOST_IP_OVERRIDE=\${SNO_DNSMASQ_IP_OVERRIDE:-"${CLUSTER_IP}"}
CLUSTER_NAME=\${SNO_CLUSTER_NAME_OVERRIDE:-"${CLUSTER_NAME}"}
BASE_DOMAIN=\${SNO_BASE_DOMAIN_OVERRIDE:-"${CLUSTER_BASE_DOMAIN}"}
CLUSTER_FULL_DOMAIN="\${CLUSTER_NAME}.\${BASE_DOMAIN}"

# Convert comma-separated IPs to array
IFS=',' read -ra HOST_IPS <<< "\$HOST_IP_OVERRIDE"

export BASE_RESOLV_CONF=/run/NetworkManager/resolv.conf
if [ "\$2" = "dhcp4-change" ] || [ "\$2" = "dhcp6-change" ] || [ "\$2" = "up" ] || [ "\$2" = "connectivity-change" ]; then
    export TMP_FILE=\$(mktemp /etc/forcedns_resolv.conf.XXXXXX)
    cp  \$BASE_RESOLV_CONF \$TMP_FILE
    chmod --reference=\$BASE_RESOLV_CONF \$TMP_FILE
    sed -i -e "s/\${CLUSTER_FULL_DOMAIN}//" \
        -e "s/search /& \${CLUSTER_FULL_DOMAIN} /" \$TMP_FILE
    
    # Add nameserver entries for all IPs (primary IP first)
    for ip in "\${HOST_IPS[@]}"; do
        # Trim whitespace
        ip=\$(echo "\$ip" | xargs)
        if [[ -n "\$ip" ]]; then
            sed -i -e "0,/nameserver/s/nameserver/& \$ip\n&/" \$TMP_FILE
        fi
    done
    
    mv \$TMP_FILE /etc/resolv.conf
fi
EOF
}

function generate_single_node_conf {
    cat <<EOF

[main]
rc-manager=unmanaged
EOF
}

function generate_dnsmasq_body {
    cat <<EOF
metadata:
  labels:
    machineconfiguration.openshift.io/role: master
  name: 50-master-dnsmasq-configuration
EOF
    if [ "${ZTP_DEPLOY_WAVE}" -gt 0 ]; then
        cat <<EOF
  annotations:
    ran.openshift.io/ztp-deploy-wave: "${ZTP_DEPLOY_WAVE}"
EOF
    fi
    cat <<EOF
spec:
  config:
    ignition:
      version: 3.1.0
    storage:
      files:
        - contents:
            source: data:text/plain;charset=utf-8;base64,$(generate_dnsmasq_content | base64 -w 0)
          mode: 365
          path: /usr/local/bin/dnsmasq_config.sh
          overwrite: true
        - contents:
            source: data:text/plain;charset=utf-8;base64,$(generate_forcedns | base64 -w 0)
          mode: 365
          path: /etc/NetworkManager/dispatcher.d/forcedns
          overwrite: true
        - contents:
            source: data:text/plain;charset=utf-8;base64,$(generate_single_node_conf | base64 -w 0)
          mode: 420
          path: /etc/NetworkManager/conf.d/single-node.conf
          overwrite: true
    systemd:
      units:
        - name: dnsmasq.service
          enabled: true
          contents: |
            [Unit]
            Description=Run dnsmasq to provide local dns for Single Node OpenShift
            Before=kubelet.service crio.service
            After=network.target

            [Service]
            TimeoutStartSec=30
            ExecStartPre=/usr/local/bin/dnsmasq_config.sh
            ExecStart=/usr/sbin/dnsmasq -k
            Restart=always

            [Install]
            WantedBy=multi-user.target
EOF
}

function generate_dnsmasq_policy {
    cat <<EOF
    # Override 50-master-dnsmasq-configuration
    - fileName: MachineConfigGeneric.yaml
      policyName: "config-policy"
      complianceType: mustonlyhave # This is to update array entry as opposed to appending a new entry.
EOF
    generate_dnsmasq_body | sed 's/^/      /'
}

function generate_dnsmasq_machine_config {
    cat <<EOF
apiVersion: machineconfiguration.openshift.io/v1
kind: MachineConfig
EOF
    generate_dnsmasq_body
}

#
# Process cmdline arguments
#

longopts=(
    "help"
    "name:"
    "domain:"
    "ip:"
    "ips:"
    "mc"
    "wave:"
)

longopts_str=$(IFS=,; echo "${longopts[*]}")

if ! OPTS=$(getopt -o "hn:d:i:mw:" --long "${longopts_str}" --name "$0" -- "$@"); then
    usage
    # shellcheck disable=SC2317
    exit 1
fi

eval set -- "${OPTS}"

while :; do
    case "$1" in
        -n|--name)
            CLUSTER_NAME="${2}"
            shift 2
            ;;
        -d|--domain)
            CLUSTER_BASE_DOMAIN="${2}"
            shift 2
            ;;
        -i|--ip)
            CLUSTER_IP="${2}"
            shift 2
            ;;
        --ips)
            IFS=',' read -ra CLUSTER_IPS <<< "${2}"
            shift 2
            ;;
        -m|--mc)
            GENERATE="machine-config"
            shift
            ;;
        -w|--wave)
            ZTP_DEPLOY_WAVE="${2}"
            shift 2
            ;;
        --)
            shift
            break
            ;;
        -h|--help)
            usage
            ;;
        *)
            usage
            ;;
    esac
done

if [ -z "${CLUSTER_NAME}" ] || [ -z "${CLUSTER_BASE_DOMAIN}" ]; then
    usage
fi

if [ -z "${CLUSTER_IP}" ] && [ ${#CLUSTER_IPS[@]} -eq 0 ]; then
    usage
fi

# Convert single IP to multi-IP format for consistency
if [ -n "${CLUSTER_IP}" ] && [ ${#CLUSTER_IPS[@]} -eq 0 ]; then
    CLUSTER_IPS=("${CLUSTER_IP}")
fi

# Join IPs with commas for the override variable
if [ ${#CLUSTER_IPS[@]} -gt 0 ]; then
    CLUSTER_IP=$(IFS=','; echo "${CLUSTER_IPS[*]}")
fi

case "${GENERATE}" in
    site-config)
        generate_dnsmasq_policy
        ;;
    machine-config)
        generate_dnsmasq_machine_config
        ;;
    *)
        usage
        ;;
esac

