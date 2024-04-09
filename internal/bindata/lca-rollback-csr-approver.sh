#!/bin/bash
#
# This utility is installed by the lifecycle-agent during an upgrade to handle
# the scenario where control plane certificates are expired on the original stateroot
# when rolling back. It is setup as a service-unit in the original stateroot during
# the LCA pre-pivot upgrade handler so that it only runs on a rollback, and is removed
# by the LCA rollback completion handler.
#
# From the original stateroot point of view, a rollback effectively just a recovery from
# having the node out-of-service for a possibly extended period of time. Especially when
# running an IBU within the first 24 hours of deploying a cluster, this means the control
# plane certificates for the original release may be expired when the rollback is triggered.
#
# Once launched, this utility will poll for Pending CSRs and approve them. This will ensure
# the control plane will be able to recover and schedule pods, allowing LCA to then complete
# the rollback. The LCA rollback completion handler will then shutdown, disable, and delete
# this service-unit and script.
#
# For reference on approving pending CSRs, see:
# https://access.redhat.com/documentation/en-us/openshift_container_platform/4.15/html/machine_management/adding-rhel-compute#installation-approve-csrs_adding-rhel-compute

declare PROG=
PROG=$(basename "$0")

# shellcheck source=/dev/null
source /etc/kubernetes/static-pod-resources/etcd-certs/configmaps/etcd-scripts/etcd-common-tools

function log {
    echo "${PROG}: $*"
}

function approve_pending_csrs {
    mapfile -t csrs < <( oc get csr -o go-template='{{range .items}}{{if not .status}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' 2>/dev/null )
    if [ ${#csrs[@]} -gt 0 ]; then
        log "Found ${#csrs[@]} pending CSRs"
        for csr in "${csrs[@]}"; do
            log "Approving CSR: ${csr}"
            oc adm certificate approve "${csr}"
        done
    fi
}

# LCA will shutdown this service-unit as part of rollback completion. Until then, loop over csr approvals
while :; do
    approve_pending_csrs
    sleep 20
done

