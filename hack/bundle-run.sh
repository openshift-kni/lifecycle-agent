#!/bin/bash
#
# bundle-run.sh installs the operator bundle using operator-sdk and works around
# an operator-sdk/OLM issue on IPv6 clusters where CatalogSource.spec.address
# may be rendered without brackets (e.g. "fd00::1:50051" instead of "[fd00::1]:50051"),
# which causes OLM resolution errors.
#
# Usage (typically via make):
#   OPERATOR_SDK=/path/to/operator-sdk BUNDLE_IMG=quay.io/... \
#     bash hack/bundle-run.sh
#
# Optional env vars:
#   INSTALL_MODE: ownnamespace (default) or allnamespaces
#   LCA_NAMESPACE: OwnNamespace install namespace (default: openshift-lifecycle-agent)
#   OPERATORS_NAMESPACE: AllNamespaces install namespace (default: openshift-operators)
#   BUNDLE_NAMESPACE: override install namespace (derived from INSTALL_MODE if unset)
#   OPERATORGROUP_NAME: OperatorGroup name for allnamespaces (default: global-operators)
#   OPERATOR_SDK: path to operator-sdk (default: <repo>/bin/operator-sdk)
#   CATALOGSOURCE_NAME: CatalogSource name created by operator-sdk (default: lifecycle-agent-catalog)
#   PATCH_TIMEOUT_SECONDS: how long to wait for CatalogSource/address to appear (default: 120)
#   BUNDLE_CLEAN_BEFORE_INSTALL: run hack/bundle-clean.sh first if true (default: false)

set -euo pipefail
if [[ "${TRACE:-0}" == "1" ]]; then
    set -x
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

: "${INSTALL_MODE:=ownnamespace}"
: "${LCA_NAMESPACE:=openshift-lifecycle-agent}"
: "${OPERATORS_NAMESPACE:=openshift-operators}"
: "${OPERATORGROUP_NAME:=global-operators}"
: "${CATALOGSOURCE_NAME:=lifecycle-agent-catalog}"
: "${PATCH_TIMEOUT_SECONDS:=120}"
: "${BUNDLE_CLEAN_BEFORE_INSTALL:=false}"
: "${OPERATOR_SDK:=${PROJECT_DIR}/bin/operator-sdk}"

case "${INSTALL_MODE}" in
    ownnamespace)
        BUNDLE_NAMESPACE="${BUNDLE_NAMESPACE:-${LCA_NAMESPACE}}"
        OLM_INSTALL_MODE="OwnNamespace"
        ;;
    allnamespaces)
        BUNDLE_NAMESPACE="${BUNDLE_NAMESPACE:-${OPERATORS_NAMESPACE}}"
        OLM_INSTALL_MODE="AllNamespaces"
        ;;
    *)
        echo "ERROR: INSTALL_MODE must be 'ownnamespace' or 'allnamespaces', got: ${INSTALL_MODE}"
        exit 2
        ;;
esac

if [[ ! -x "${OPERATOR_SDK}" ]]; then
    echo "ERROR: OPERATOR_SDK is not executable: ${OPERATOR_SDK}"
    exit 2
fi

if [[ -z "${BUNDLE_IMG:-}" ]]; then
    echo "ERROR: BUNDLE_IMG is not set"
    exit 2
fi

if [[ "${BUNDLE_CLEAN_BEFORE_INSTALL}" == "true" ]]; then
    INSTALL_MODE="${INSTALL_MODE}" \
        LCA_NAMESPACE="${LCA_NAMESPACE}" \
        OPERATORS_NAMESPACE="${OPERATORS_NAMESPACE}" \
        CATALOGSOURCE_NAME="${CATALOGSOURCE_NAME}" \
        OPERATOR_SDK="${OPERATOR_SDK}" \
        BUNDLE_NAMESPACE="${BUNDLE_NAMESPACE}" \
        bash "$(dirname "${BASH_SOURCE[0]}")/bundle-clean.sh"
fi

echo "Installing lifecycle-agent bundle (INSTALL_MODE=${INSTALL_MODE}, olm=${OLM_INSTALL_MODE}, namespace=${BUNDLE_NAMESPACE})"

oc create ns "${BUNDLE_NAMESPACE}" 2>/dev/null || true

"${OPERATOR_SDK}" --security-context-config restricted -n "${BUNDLE_NAMESPACE}" \
    run bundle "${BUNDLE_IMG}" --install-mode="${OLM_INSTALL_MODE}" &
sdk_pid=$!

for _i in $(seq 1 "${PATCH_TIMEOUT_SECONDS}"); do
    if ! oc get "catalogsource/${CATALOGSOURCE_NAME}" -n "${BUNDLE_NAMESPACE}" >/dev/null 2>&1; then
        sleep 1
        continue
    fi

    addr="$(oc get "catalogsource/${CATALOGSOURCE_NAME}" -n "${BUNDLE_NAMESPACE}" -o jsonpath='{.spec.address}' 2>/dev/null || true)"
    if [[ -z "${addr}" ]]; then
        # CatalogSource can exist briefly before spec.address is populated.
        sleep 1
        continue
    fi

    # Already bracketed -> nothing to do.
    case "${addr}" in
        \[*\]*) break ;;
    esac

    # Patch unbracketed IPv6 "host:port" (port must be numeric).
    if [[ "${addr}" =~ ^([0-9A-Fa-f:]+):([0-9]+)$ ]]; then
        host="${BASH_REMATCH[1]}"
        port="${BASH_REMATCH[2]}"
        newaddr="[${host}]:${port}"
        echo "Patching catalogsource/${CATALOGSOURCE_NAME} in ${BUNDLE_NAMESPACE} spec.address: ${addr} -> ${newaddr}"
        patch="$(printf '{"spec":{"address":"%s"}}' "${newaddr}")"
        oc patch "catalogsource/${CATALOGSOURCE_NAME}" -n "${BUNDLE_NAMESPACE}" --type=merge -p "${patch}"
    fi
    break
done

wait "${sdk_pid}"
