#!/bin/bash
#
# bundle-clean.sh removes a lifecycle-agent operator-sdk bundle install.
#
# Usage (typically via make):
#   INSTALL_MODE=ownnamespace|allnamespaces bash hack/bundle-clean.sh
#
# Optional env vars:
#   INSTALL_MODE: ownnamespace (default) or allnamespaces
#   LCA_NAMESPACE: default install namespace (default: openshift-lifecycle-agent)
#   BUNDLE_NAMESPACE: override install namespace
#   CATALOGSOURCE_NAME: CatalogSource name (default: lifecycle-agent-catalog)
#   OPERATORGROUP_NAME: OperatorGroup name for allnamespaces (default: global-operators)
#   OPERATOR_SDK: path to operator-sdk (default: <repo>/bin/operator-sdk)

set -euo pipefail
if [[ "${TRACE:-0}" == "1" ]]; then
    set -x
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

: "${INSTALL_MODE:=ownnamespace}"
: "${LCA_NAMESPACE:=openshift-lifecycle-agent}"
: "${OPERATORGROUP_NAME:=global-operators}"
: "${CATALOGSOURCE_NAME:=lifecycle-agent-catalog}"
: "${OPERATOR_SDK:=${PROJECT_DIR}/bin/operator-sdk}"

case "${INSTALL_MODE}" in
    ownnamespace|allnamespaces) ;;
    *)
        echo "ERROR: INSTALL_MODE must be 'ownnamespace' or 'allnamespaces', got: ${INSTALL_MODE}"
        exit 2
        ;;
esac

BUNDLE_NAMESPACE="${BUNDLE_NAMESPACE:-${LCA_NAMESPACE}}"

echo "Uninstalling lifecycle-agent (INSTALL_MODE=${INSTALL_MODE}, namespace=${BUNDLE_NAMESPACE})"

while IFS= read -r sub; do
    [[ -z "${sub}" ]] && continue
    oc delete "${sub}" -n "${BUNDLE_NAMESPACE}" --ignore-not-found
done < <(oc get subscription -n "${BUNDLE_NAMESPACE}" -o name 2>/dev/null | grep lifecycle-agent || true)

oc delete "catalogsource/${CATALOGSOURCE_NAME}" -n "${BUNDLE_NAMESPACE}" --ignore-not-found
oc delete pod -n "${BUNDLE_NAMESPACE}" -l "olm.catalogSource=${CATALOGSOURCE_NAME}" --ignore-not-found

if [[ -n "${OPERATOR_SDK:-}" && -x "${OPERATOR_SDK}" ]]; then
    "${OPERATOR_SDK}" cleanup lifecycle-agent -n "${BUNDLE_NAMESPACE}" 2>/dev/null || true
fi

# operator-sdk run bundle creates operator-sdk-og; legacy OwnNamespace installs may use global-operators.
oc delete operatorgroup operator-sdk-og -n "${BUNDLE_NAMESPACE}" --ignore-not-found
if [[ "${INSTALL_MODE}" == "ownnamespace" ]]; then
    oc delete operatorgroup "${OPERATORGROUP_NAME}" -n "${BUNDLE_NAMESPACE}" --ignore-not-found
fi

if [[ "${INSTALL_MODE}" == "ownnamespace" || "${BUNDLE_NAMESPACE}" == "${LCA_NAMESPACE}" ]]; then
    oc delete ns "${BUNDLE_NAMESPACE}" --ignore-not-found
else
    echo "Keeping install namespace ${BUNDLE_NAMESPACE}: not the default LCA namespace in AllNamespaces mode."
fi

if oc get pods -A 2>/dev/null | grep -q lifecycle-agent; then
    echo "WARNING: lifecycle-agent pods may still be terminating"
    oc get pods -A | grep lifecycle-agent || true
else
    echo "No lifecycle-agent pods remain."
fi
