#!/bin/bash
#

PROG=$(basename "$0")

declare CLUSTER_NAME_DOMAIN=
declare CLUSTER_IP=
declare GENERATE="site-config"

function usage {
    cat <<EOF
Usage: ${PROG} --name <cluster.domain> --ip <addr> [ --mc ]
Options:
    --name <cluster.domain> - Cluster name + baseDomain
    --ip   <addr>           - Node IP
    --mc                    - Generate machine-config only

Summary:
    Generates a subsection of site-policy to include dnsmasq config for an SNO.

Example:
    ${PROG} --name cnfde8.sno.ptp.lab.eng.bos.redhat.com --ip 10.16.231.8
EOF
    exit 1
}

function generate_single_node_conf_template {
    cat <<EOF

address=/apps.${CLUSTER_NAME_DOMAIN}/HOST_IP
address=/api-int.${CLUSTER_NAME_DOMAIN}/HOST_IP
address=/api.${CLUSTER_NAME_DOMAIN}/HOST_IP
EOF
}

function generate_forcedns {
    cat <<EOF
#!/bin/bash
export IP="${CLUSTER_IP}"
if [ -f /etc/default/node-ip ]; then
    export IP=\$(cat /etc/default/node-ip)
fi
export BASE_RESOLV_CONF=/run/NetworkManager/resolv.conf
if [ "\$2" = "dhcp4-change" ] || [ "\$2" = "dhcp6-change" ] || [ "\$2" = "up" ] || [ "\$2" = "connectivity-change" ]; then
    export TMP_FILE=\$(mktemp /etc/forcedns_resolv.conf.XXXXXX)
    cp  \$BASE_RESOLV_CONF \$TMP_FILE
    chmod --reference=\$BASE_RESOLV_CONF \$TMP_FILE
    sed -i -e "s/${CLUSTER_NAME_DOMAIN}//" \\
        -e "s/search /& ${CLUSTER_NAME_DOMAIN} /" \\
        -e "0,/nameserver/s/nameserver/& \$IP\n&/" \$TMP_FILE
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
apiVersion: machineconfiguration.openshift.io/v1
kind: MachineConfig
metadata:
  labels:
    machineconfiguration.openshift.io/role: master
  name: 50-master-dnsmasq-configuration
spec:
  config:
    storage:
      files:
        - contents:
            source: data:text/plain;charset=utf-8;base64,$(generate_single_node_conf_template | base64 -w 0)
          mode: 420
          path: /etc/default/single-node.conf_template
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
            After=network.target nodeip-configuration.service
            [Service]
            TimeoutStartSec=30
            ExecStartPre=/bin/bash -c 'until [ -e /var/run/nodeip-configuration/primary-ip ]; do sleep 1; done'
            ExecStartPre=/bin/bash -c 'sed "s/HOST_IP/\$(cat /var/run/nodeip-configuration/primary-ip)/g" /etc/default/single-node.conf_template > /etc/dnsmasq.d/single-node.conf'
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
    "ip:"
    "mc"
)

longopts_str=$(IFS=,; echo "${longopts[*]}")

if ! OPTS=$(getopt -o "hn:i:m" --long "${longopts_str}" --name "$0" -- "$@"); then
    usage
    exit 1
fi

eval set -- "${OPTS}"

while :; do
    case "$1" in
        -n|--name)
            CLUSTER_NAME_DOMAIN="${2}"
            shift 2
            ;;
        -i|--ip)
            CLUSTER_IP="${2}"
            shift 2
            ;;
        -m|--mc)
            GENERATE="machine-config"
            shift
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

if [ -z "${CLUSTER_NAME_DOMAIN}" ] || [ -z "${CLUSTER_IP}" ]; then
    usage
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

