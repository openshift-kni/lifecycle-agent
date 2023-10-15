#!/bin/bash
#
# Image Based Upgrade Prep - Setup new stateroot
#

declare SEED_IMAGE=
declare PROGRESS_FILE=
declare WORKDIR=

function usage {
    cat <<ENDUSAGE
Parameters:
    --seed-image <image>
    --progress-file <file>
ENDUSAGE
    exit 1
}

function set_progress {
    echo "$1" > "${PROGRESS_FILE}"
}

function fatal {
    set_progress "Failed"
    echo "$@" >&2
    exit 1
}

function cleanup {
    if [ -n "${WORKDIR}" ]; then
        rm -rf "${WORKDIR}"
    fi
}

trap cleanup EXIT

function log_it {
    echo "$@" | tr '[:print:]' -
    echo "$@"
    echo "$@" | tr '[:print:]' -
}

function build_kargs {
    jq -r '.spec.kernelArguments[]' "${img_mnt}/mco-currentconfig.json" \
        | xargs --no-run-if-empty -I% echo -n "--karg-append % "
}

LONGOPTS="seed-image:,progress-file:"
OPTS=$(getopt -o h --long "${LONGOPTS}" --name "$0" -- "$@")

eval set -- "${OPTS}"

while :; do
    case "$1" in
        --seed-image)
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

set_progress "started-stateroot"

WORKDIR=$(mktemp -d -p /var/tmp)
if [ -z "${WORKDIR}" ]; then
    fatal "Failed to create workdir"
fi

export KUBECONFIG=/etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/lb-ext.kubeconfig

mount /sysroot -o remount,rw || fatal "Failed to remount /sysroot"

# Image should already be pulled and mounted
img_mnt=$(podman image mount --format json | jq -r --arg img "${SEED_IMAGE}" '.[] | select(.Repositories[0] == $img) | .mountpoint')
if [ -z "${img_mnt}" ]; then
    fatal "Seed image is not mounted: ${SEED_IMAGE}"
fi

ostree_repo="${WORKDIR}/ostree"
mkdir -p "${ostree_repo}" || fatal "Failed to create dir: ${ostree_repo}"

tar xzf "${img_mnt}/ostree.tgz" --selinux -C "${ostree_repo}"

# Collect seed deployment data from the backup
upg_booted_id=$(jq -r '.deployments[] | select(.booted == true) | .id' "${img_mnt}/rpm-ostree.json")
upg_booted_deployment=${upg_booted_id/*-}
upg_booted_ref=${upg_booted_deployment/\.*}

if [ -z "${upg_booted_id}" ] || [ -z "${upg_booted_deployment}" ] || [ -z "${upg_booted_ref}" ]; then
    fatal "Failed to identify deployment from seed image"
fi

up_ver=$(jq -r '.status.desired.version' "${img_mnt}/clusterversion.json")
if [ -z "${up_ver}" ]; then
    fatal "Failed to identify version from seed image"
fi

new_osname="rhcos_${up_ver}"

log_it "Importing remote ostree"
ostree pull-local "${ostree_repo}" || fatal "Failed: ostree pull-local ${ostree_repo}"
ostree admin os-init "${new_osname}" || fatal "Failed: ostree admin os-init ${new_osname}"

log_it "Creating new deployment ${new_osname}"
# We should create the new deploy as not-default, and after the whole process is done, be able to switch to it for the next reboot
# ostree admin deploy --os ${new_osname} $(build_kargs) --not-as-default ${upg_booted_ref}
# Until I find how to do the switch, I'll deploy as default
# shellcheck disable=SC2046
ostree admin deploy --os "${new_osname}" $(build_kargs) "${upg_booted_ref}" || fatal "Failed ostree admin deploy"
ostree_deploy=$(ostree admin status | awk /"${new_osname}"/'{print $2}')
if [ -z "${ostree_deploy}" ]; then
    fatal "Unable to determine deployment"
fi

# Restore the seed .origin file
cp "${img_mnt}/ostree-${upg_booted_deployment}.origin" "/ostree/deploy/${new_osname}/deploy/${ostree_deploy}.origin" || fatal "Failed to copy .origin from seed"

log_it "Restoring /var"
tar xzf "${img_mnt}/var.tgz" -C "/ostree/deploy/${new_osname}" --selinux || fatal "Failed to restore /var"

log_it "Restoring /etc"
tar xzf "${img_mnt}/etc.tgz" -C "/ostree/deploy/${new_osname}/deploy/${ostree_deploy}" --selinux || fatal "Failed to restore /etc"
xargs --no-run-if-empty -ifile rm -f /ostree/deploy/"${new_osname}"/deploy/"${ostree_deploy}"/file < "${img_mnt}/etc.deletions"

log_it "Waiting for API"
until oc get clusterversion &>/dev/null; do
    sleep 5
done

log_it "Backing up certificates to be used by recert"
certs_dir="/ostree/deploy/${new_osname}/var/opt/openshift/certs"
mkdir -p "${certs_dir}"
oc extract -n openshift-config configmap/admin-kubeconfig-client-ca --keys=ca-bundle.crt --to=- > "${certs_dir}/admin-kubeconfig-client-ca.crt" \
    || fatal "Failed: oc extract -n openshift-config configmap/admin-kubeconfig-client-ca --keys=ca-bundle.crt"
for key in {loadbalancer,localhost,service-network}-serving-signer; do
    oc extract -n openshift-kube-apiserver-operator secret/${key} --keys=tls.key --to=- > "${certs_dir}/${key}.key" \
        || fatal "Failed: oc extract -n openshift-kube-apiserver-operator secret/${key} --keys=tls.key"
done
ingress_cn=$(oc extract -n openshift-ingress-operator secret/router-ca --keys=tls.crt --to=- | openssl x509 -subject -noout -nameopt multiline | awk '/commonName/{print $3}')
if [ -z "${ingress_cn}" ]; then
    fatal "Failed to get ingress_cn"
fi
oc extract -n openshift-ingress-operator secret/router-ca --keys=tls.key --to=- > "${certs_dir}/ingresskey-${ingress_cn}" \
    || fatal "Failed: oc extract -n openshift-ingress-operator secret/router-ca --keys=tls.key"

set_progress "completed-stateroot"

exit 0
