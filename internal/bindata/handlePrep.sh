#!/bin/bash
#
# Image Based Upgrade prep script
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
    if [ -n "${SEED_IMAGE}" ]; then
        if podman image exists "${SEED_IMAGE}"; then
            podman image unmount "${SEED_IMAGE}"
            podman rmi "${SEED_IMAGE}"
        fi
    fi

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

function build_catalog_regex {
    if grep -q . "${img_mnt}/catalogimages.list"; then
        awk -F: '{print $1 ":"; print $1 "@";}' "${img_mnt}/catalogimages.list" | paste -sd\|
    fi
}

LONGOPTS="seed-image:,progress-file:"
OPTS=$(getopt -o h --long "${LONGOPTS}" --name "$0" -- "$@")

eval set -- "${OPTS}"

while :; do
    case "$1" in
        --seed-image)
            # shellcheck disable=SC2034
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

set_progress "Starting"

WORKDIR=$(mktemp -d -p /var/tmp)
if [ -z "${WORKDIR}" ]; then
    fatal "Failed to create workdir"
fi

export KUBECONFIG=/etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/lb-ext.kubeconfig

mount /sysroot -o remount,rw || fatal "Failed to remount /sysroot"

# Import OCIs
log_it "Pulling and mounting seed OCI"
podman pull "${SEED_IMAGE}" || fatal "Failed to pull image: ${SEED_IMAGE}"

img_mnt=$(podman image mount "${SEED_IMAGE}")
if [ -z "${img_mnt}" ]; then
    fatal "Failed to mount image: ${SEED_IMAGE}"
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

log_it "Precaching non-catalog images"
grep -vE "$(build_catalog_regex)" "${img_mnt}/containers.list" | xargs --no-run-if-empty --max-args 1 --max-procs 10 crictl pull

log_it "Precaching catalog images"
if grep -q . "${img_mnt}/catalogimages.list"; then
    xargs --no-run-if-empty --max-args 1 --max-procs 10 crictl pull < "${img_mnt}/catalogimages.list"
fi

# NOTE: This could be part of the post-reboot config, or just handle it ahead of the reboot
# Copy over /etc/ssh keys as well? /etc/hostname?
# Optionally, the B-side systemd service could reach back to the A-side /etc content, as long as there's only 2 ostree deployments.
# Or the A-side can populate a file to let the B-side systemd service know what the A-side deployment is... which might also be handy for rollbacks
#
log_it "Update networking config"
rm -f /sysroot/ostree/deploy/"${new_osname}"/deploy/"${ostree_deploy}"/etc/NetworkManager/system-connections/*
cp --preserve=all /etc/NetworkManager/system-connections/* \
    /sysroot/ostree/deploy/"${new_osname}"/deploy/"${ostree_deploy}"/etc/NetworkManager/system-connections/ \
    || fatal "Failed to copy networking config"

log_it "DONE. Be sure to attach the relocation site info to the host (either via ISO or make copy-config) and you can reboot the node"

set_progress "Completed"

exit 0
