#!/usr/bin/env bash
# shellcheck disable=SC2140
set -euoE pipefail ## -E option will cause functions to inherit trap
set -x ## Be more verbose

echo "Reconfiguring single node OpenShift"

mkdir -p /opt/openshift
cd /opt/openshift

EXTRA_MANIFESTS_PATH=/opt/openshift/extra-manifests
RELOCATION_CONFIG_PATH=/opt/openshift/cluster-configuration

# Temporary - get from manifest.json or collected config, etc
SEED_CLUSTER_NAME=cnfde8
SEED_DNS_DOMAIN=sno.ptp.lab.eng.bos.redhat.com
SNO_CLUSTER_NAME=cnfdf01
SNO_DNS_DOMAIN=sno.telco5gran.eng.rdu2.redhat.com
# Shouldn't need seed hostname
# SEED_HOSTNAME=cnfde8.ptp.lab.eng.bos.redhat.com

NODE_IP=$(cat /etc/default/node-ip)

# Replace this with a function that loads values from yaml file
set +o allexport

# Recertify
function recert {
    OLD_IP=$(cat /etc/default/seed-ip)

    ETCD_IMAGE="$(jq -r '.spec.containers[] | select(.name == "etcd") | .image' </etc/kubernetes/manifests/etcd-pod.yaml)"
    #RECERT_IMAGE="quay.io/edge-infrastructure/recert:latest"
    RECERT_IMAGE="quay.io/otuchfel/recert:donpub"
    #RECERT_IMAGE="quay.io/dpenney/poc:recert"
    local certs_dir=/var/opt/openshift/certs
    local recert_cmd="sudo podman run \
        --pull=always \
        --name recert \
        --network=host \
        --privileged \
        --replace \
        -v /var/opt/openshift:/var/opt/openshift \
        -v /etc/kubernetes:/kubernetes \
        -v /var/lib/kubelet:/kubelet \
        -v /etc:/host-etc \
        ${RECERT_IMAGE} \
        --etcd-endpoint localhost:2379 \
        --static-dir /kubernetes \
        --static-dir /kubelet \
        --static-dir /host-etc/machine-config-daemon \
        --static-file /host-etc/mcs-machine-config-content.json \
        --cn-san-replace api-int.${SEED_CLUSTER_NAME}.${SEED_DNS_DOMAIN}:api-int.${SNO_CLUSTER_NAME}.${SNO_DNS_DOMAIN} \
        --cn-san-replace api.${SEED_CLUSTER_NAME}.${SEED_DNS_DOMAIN}:api.${SNO_CLUSTER_NAME}.${SNO_DNS_DOMAIN} \
        --cn-san-replace *.apps.${SEED_CLUSTER_NAME}.${SEED_DNS_DOMAIN}:*.apps.${SNO_CLUSTER_NAME}.${SNO_DNS_DOMAIN} \
        --cluster-rename ${SNO_CLUSTER_NAME}:${SNO_DNS_DOMAIN} \
        --summary-file /kubernetes/recert-summary.yaml \
        --extend-expiration"

    sudo podman run \
        --authfile=/var/lib/kubelet/config.json \
        --name recert_etcd \
        --detach \
        --rm \
        --network=host \
        --privileged \
        --replace \
        --entrypoint etcd \
        -v /var/lib/etcd:/store \
        ${ETCD_IMAGE} \
        --name editor \
        --data-dir /store
    sleep 10 # TODO: wait for etcd

    # Recert node ip in order to support single ip
    # remove this condition when we will support only single ip
    if [[ -n "${NODE_IP}" ]]; then
        recert_cmd="${recert_cmd} ""--cn-san-replace ${OLD_IP},${NODE_IP}"
        if [[ ${NODE_IP} =~ .*:.* ]]; then
            ETCD_NEW_IP="[${NODE_IP}]"
        else
            ETCD_NEW_IP=${NODE_IP}
        fi

        sudo podman exec -it recert_etcd bash -c "/usr/bin/etcdctl member list | cut -d',' -f1 | xargs -i etcdctl member update "{}" --peer-urls=http://${ETCD_NEW_IP}:2380"
        sudo podman exec -it recert_etcd bash -c "/usr/bin/etcdctl del /kubernetes.io/configmaps/openshift-etcd/etcd-endpoints"
        find /etc/kubernetes/ -type f -print0 | xargs -0 sed -i "s/${OLD_IP}/${NODE_IP}/g"
    fi

    # Use previous cluster certs if directory is present
    if [[ -d $certs_dir ]]; then
        if sudo podman container exists recert; then
            sudo podman rm -f recert
        fi
        ingress_key=$(readlink -f $certs_dir/ingresskey-*)
        ingress_cn=$(basename $ingress_key | cut -d - -f 2-)
        $recert_cmd \
            --use-cert $certs_dir/admin-kubeconfig-client-ca.crt \
            --use-key "kube-apiserver-lb-signer $certs_dir/loadbalancer-serving-signer.key" \
            --use-key "kube-apiserver-localhost-signer $certs_dir/localhost-serving-signer.key" \
            --use-key "kube-apiserver-service-network-signer $certs_dir/service-network-serving-signer.key" \
            --use-key "$ingress_cn $ingress_key"
    else
        $recert_cmd
    fi

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
    # Wait until the list of nodes has at least one
    until oc get nodes -ojsonpath='{.items[0].metadata.name}' &> /dev/null; do
        echo "Waiting for node ..."
        sleep 5
    done
    echo "api is available"
}

wait_for_api

# Workaround for MCO issue
oc patch controllerconfigs.machineconfiguration.openshift.io machine-config-controller -p='
    {
        "spec": {
            "infra": {
                "status": {
                    "apiServerURL": "https://api.'"${SNO_CLUSTER_NAME}.${SNO_DNS_DOMAIN}"':6443",
                    "apiServerInternalURI": "https://api-int.'"${SNO_CLUSTER_NAME}.${SNO_DNS_DOMAIN}"':6443"
                }
            },
            "dns": {
                "spec": {
                    "baseDomain": "'"${SNO_CLUSTER_NAME}.${SNO_DNS_DOMAIN}"'"
                }
            }
        }
    }
' --type=merge

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

# if hostname has changed
if [[ "$(oc get nodes -ojsonpath='{.items[0].metadata.name}')" != "$(hostname)" ]]; then
    wait_approve_csr "kube-apiserver-client-kubelet"
    wait_approve_csr "kubelet-serving"

    echo "Deleting previous node..."
    oc delete node "$(oc get nodes -ojsonpath='{.items[?(@.metadata.name != "'"$(hostname)"'")].metadata.name}')"
fi

verify_csr_subject() {
    local csr=${1}
    local subject

    subject="$(oc get csr -ojsonpath='{.spec.request}' "${csr}" |base64 -d | openssl req -noout -subject -nameopt multiline)"

    if [ "$(echo "${subject}" |grep commonName |awk '{print $3}')" != "system:node:$(hostname)" ]; then
        echo "CommonName is not 'system:node:master1'"
        return
    fi

    if [ "$(echo "${subject}" |grep organizationName |awk '{print $3}')" != "system:nodes" ]; then
        echo "Organization is not 'system:nodes'"
        return
    fi
}

# If the kubelet API server client certificate has expired:
#   1. wait for the respective CSR to be created
#   2. verify it
#   3. approve it
#   4. wait for the certificate to be issued
KUBELET_CLIENT_CERTIFICATE=/var/lib/kubelet/pki/kubelet-client-current.pem
until openssl x509 -in ${KUBELET_CLIENT_CERTIFICATE} -checkend 30 &> /dev/null; do
    echo "${KUBELET_CLIENT_CERTIFICATE} has expired, waiting for new one to be issued..."

    csr=$(oc get csr -o go-template='{{range .items}}{{if and (not .status) (eq .spec.signerName "kubernetes.io/kube-apiserver-client-kubelet") (eq .spec.username "system:serviceaccount:openshift-machine-config-operator:node-bootstrapper")}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | head -1 || true)
    if [ -z "${csr}" ]; then
        sleep 5
        continue
    fi

    echo "${csr} is pending. Verifying CSR before approving it..."
    err=$(verify_csr_subject "${csr}")
    if [ -n "${err}" ]; then
        echo "${csr} could not be verified: ${err}"
        break
    fi

    echo "${csr} successfully verified. Approving it..."
    oc adm certificate approve "${csr}"
done
echo "${KUBELET_CLIENT_CERTIFICATE} is valid."

# Reconfigure DNS
echo "Updating dnsmasq with new domain"
cat << EOF > /etc/dnsmasq.d/customer-domain.conf

address=/apps.${SNO_DNS_DOMAIN}/${NODE_IP}
address=/api-int.${SNO_DNS_DOMAIN}/${NODE_IP}
address=/api.${SNO_DNS_DOMAIN}/${NODE_IP}
EOF
systemctl restart dnsmasq

if [ -d ${EXTRA_MANIFESTS_PATH} ]; then
    echo "Applying extra-manifests"
    oc apply -f "${EXTRA_MANIFESTS_PATH}"
fi

verify_csr_addresses() {
    local csr=${1}
    local addresses=${2}
    local csr_san_content

    csr_san_content=$(oc get csr -ojsonpath='{.spec.request}' "${csr}" |base64 -d | openssl req -noout -text |grep DNS)
    for address in "${addresses[@]}"; do
        if [[ "${csr_san_content}" != *"${address}"* ]]; then
            echo "${address} not in CSR DNS or IP addresses"
            return
        fi
    done
}
# If the kubelet serving certificate has expired:
#   1. wait for the respective CSR to be created
#   2. verify it
#   3. approve it
#   4. wait for the certificate to be issued
KUBELET_SERVING_CERTIFICATE=/var/lib/kubelet/pki/kubelet-server-current.pem
until openssl x509 -in ${KUBELET_SERVING_CERTIFICATE} -checkend 30 &> /dev/null; do
    echo "${KUBELET_SERVING_CERTIFICATE} has expired, waiting for new one to be issued..."

    csr=$(oc get csr -o go-template='{{range .items}}{{if and (not .status) (eq .spec.signerName "kubernetes.io/kubelet-serving") (eq .spec.username "system:node:'"$(hostname)"'")}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | head -1 || true)
    if [ -z "${csr}" ]; then
        sleep 5
        continue
    fi

    echo "${csr} is pending. Verifying CSR before approving it..."
    err=$(verify_csr_subject "${csr}")
    if [ -n "${err}" ]; then
        echo "${csr} could not be verified: ${err}"
        break
    fi

    IFS=$' ' read -r -d '' -a addresses < <( oc get nodes "$(hostname)" -ojsonpath='{.status.addresses[*].address}'  && printf '\0' )
    err=$(verify_csr_addresses "${csr}" "${addresses}")
    if [ -n "${err}" ]; then
        echo "${csr} addresses could not be verified: ${err}"
        break
    fi

    echo "${csr} successfully verified. Approving it..."
    oc adm certificate approve "${csr}"
done
echo "${KUBELET_SERVING_CERTIFICATE} is valid."

rm -rf /opt/openshift
systemctl disable installation-configuration.service
