#!/usr/bin/env bash
# shellcheck disable=SC2140
set -euoE pipefail ## -E option will cause functions to inherit trap
set -x ## Be more verbose

echo "Reconfiguring single node OpenShift"

OPT_DIR=/opt/openshift

mkdir -p ${OPT_DIR}
cd ${OPT_DIR}

EXTRA_MANIFESTS_PATH=${OPT_DIR}/extra-manifests
RELOCATION_CONFIG_PATH=${OPT_DIR}/cluster-configuration

echo "Waiting for ${RELOCATION_CONFIG_PATH}"
while [[ ! -d "${RELOCATION_CONFIG_PATH}" ]]; do
    echo "Waiting for site-config"
    sleep 5
done

if [ ! -d "${RELOCATION_CONFIG_PATH}" ]; then
    echo "Failed to find cluster configuration at ${RELOCATION_CONFIG_PATH}"
    exit 1
fi

echo "${RELOCATION_CONFIG_PATH} has been found"
# Replace this with a function that loads values from yaml file
set +o allexport

function wait_for_etcd {
    local waiting_msg="Waiting for etcd to be available..."

    echo "${waiting_msg}"
    until curl -s http://localhost:2379/health |jq -e '.health == "true"' &> /dev/null; do
        echo "${waiting_msg}"
        sleep 2
    done
    echo "etcd is now available"
}

# Recertify
function recert {
    # shellcheck disable=SC1091
    source /etc/default/sno_dnsmasq_configuration_overrides
    NODE_IP=${SNO_DNSMASQ_IP_OVERRIDE}
    OLD_IP=$(jq -r '.master_ip' "${OPT_DIR}/seed_manifest.json")

    ETCD_IMAGE="$(jq -r '.spec.containers[] | select(.name == "etcd") | .image' </etc/kubernetes/manifests/etcd-pod.yaml)"
    local recert_tool_image=${RECERT_IMAGE:-quay.io/edge-infrastructure/recert:latest}
    local recert_cmd="sudo podman run --name recert --network=host --privileged --replace -e RECERT_CONFIG="${RELOCATION_CONFIG_PATH}/recert_config.json" -v /var/tmp:/var/tmp -v /opt/openshift:/opt/openshift -v /etc/kubernetes:/kubernetes -v /var/lib/kubelet:/kubelet -v /etc/machine-config-daemon:/machine-config-daemon -v /etc:/host-etc ${recert_tool_image}"

    # run etcd
    sudo podman run --authfile=/var/lib/kubelet/config.json --name recert_etcd --detach --rm --network=host --replace --privileged --entrypoint etcd -v /var/lib/etcd:/store ${ETCD_IMAGE} --name editor --data-dir /store

    wait_for_etcd

    if [[ ${NODE_IP} =~ .*:.* ]]; then
        ETCD_NEW_IP="[${NODE_IP}]"
    else
        ETCD_NEW_IP=${NODE_IP}
    fi
    # TODO: remove after https://issues.redhat.com/browse/ETCD-503
    sudo podman exec -it recert_etcd bash -c "/usr/bin/etcdctl member list | cut -d',' -f1 | xargs -i etcdctl member update "{}" --peer-urls=http://${ETCD_NEW_IP}:2380"
    sudo podman exec -it recert_etcd bash -c "/usr/bin/etcdctl del /kubernetes.io/configmaps/openshift-etcd/etcd-endpoints"
    find /etc/kubernetes/ -type f -print0 | xargs -0 sed -i "s/${OLD_IP}/${NODE_IP}/g"

    $recert_cmd

    # TODO: uncomment this once recert is stable
    # sudo podman rm recert
    sudo podman kill recert_etcd
}

if [ ! -f recert.done ]; then
    sleep 30 # TODO: wait for weird network DHCP/DNS issue to resolve
    echo "Regenerate cryptographic keys "
    recert
    touch recert.done
fi


# TODO check if we really need to stop kubelet
echo "Starting kubelet"
systemctl enable kubelet --now

#TODO: we need to add kubeconfig to the node for the configuration stage, this kubeconfig might not suffice
export KUBECONFIG=/etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/localhost.kubeconfig
function wait_for_api {
    echo "Waiting for api ..."
    until oc get clusterversion &> /dev/null; do
        echo "Waiting for api ..."
        sleep 5
    done
    echo "api is available"
}

wait_approve_csr() {
    local name=${1}

    echo "Waiting for ${name} CSR..."
    until oc get csr | grep -i "${name}" | grep -i "pending" &> /dev/null; do
        echo "Waiting for ${name} CSR..."
        sleep 5
    done
    echo "CSR ${name} is ready for approval"

    echo "Approving all pending CSRs..."
    oc get csr -o go-template='{{range .items}}{{if not .status}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | xargs oc adm certificate approve
}

wait_for_api

# skip waiting for csrs in case node was already approved
if [[ "$(oc get nodes -o jsonpath='{range .items[0]}{.status.conditions[?(@.type=="Ready")].status}')" != "True" ]]; then
    wait_approve_csr "kube-apiserver-client-kubelet"
    wait_approve_csr "kubelet-serving"
fi

echo "Applying cluster configuration"
oc apply -f "${RELOCATION_CONFIG_PATH}/manifests"

if [ -d ${EXTRA_MANIFESTS_PATH} ]; then
    echo "Applying extra-manifests"
    oc apply -f "${EXTRA_MANIFESTS_PATH}"
fi

rm -rf /opt/openshift
systemctl disable installation-configuration.service
