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
#   LCA_NAMESPACE: default install namespace (default: openshift-lifecycle-agent)
#   BUNDLE_NAMESPACE: override install namespace
#   OPERATORGROUP_NAME: OperatorGroup name for allnamespaces (default: global-operators)
#   OPERATOR_SDK: path to operator-sdk (default: <repo>/bin/operator-sdk)
#   BUNDLE_RUN_TIMEOUT: how long to wait for the CSV to install (default: 15m)
#   CATALOGSOURCE_NAME: CatalogSource name created by operator-sdk (default: lifecycle-agent-catalog)
#   PATCH_TIMEOUT_SECONDS: how long to wait for CatalogSource/address to appear (default: 120)
#   SA_WAIT_TIMEOUT_SECONDS: how long to wait for default serviceaccount (default: 900)
#   SA_WAIT_POLL_INTERVAL_SECONDS: poll interval while waiting for serviceaccount (default: 30)
#   BUNDLE_CLEAN_BEFORE_INSTALL: run hack/bundle-clean.sh first if true (default: false)

set -euo pipefail
if [[ "${TRACE:-0}" == "1" ]]; then
    set -x
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

: "${INSTALL_MODE:=ownnamespace}"
: "${LCA_NAMESPACE:=openshift-lifecycle-agent}"
: "${OPERATORGROUP_NAME:=global-operators}"
: "${BUNDLE_RUN_TIMEOUT:=15m}"
: "${CATALOGSOURCE_NAME:=lifecycle-agent-catalog}"
: "${PATCH_TIMEOUT_SECONDS:=120}"
: "${SA_WAIT_TIMEOUT_SECONDS:=900}"
: "${SA_WAIT_POLL_INTERVAL_SECONDS:=30}"
: "${BUNDLE_CLEAN_BEFORE_INSTALL:=false}"
: "${OPERATOR_SDK:=${PROJECT_DIR}/bin/operator-sdk}"

case "${INSTALL_MODE}" in
    ownnamespace)
        OLM_INSTALL_MODE="OwnNamespace"
        ;;
    allnamespaces)
        OLM_INSTALL_MODE="AllNamespaces"
        ;;
    *)
        echo "ERROR: INSTALL_MODE must be 'ownnamespace' or 'allnamespaces', got: ${INSTALL_MODE}"
        exit 2
        ;;
esac

BUNDLE_NAMESPACE="${BUNDLE_NAMESPACE:-${LCA_NAMESPACE}}"

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
        CATALOGSOURCE_NAME="${CATALOGSOURCE_NAME}" \
        OPERATOR_SDK="${OPERATOR_SDK}" \
        BUNDLE_NAMESPACE="${BUNDLE_NAMESPACE}" \
        bash "$(dirname "${BASH_SOURCE[0]}")/bundle-clean.sh"
fi

echo "Installing lifecycle-agent bundle (INSTALL_MODE=${INSTALL_MODE}, olm=${OLM_INSTALL_MODE}, namespace=${BUNDLE_NAMESPACE})"

oc create ns "${BUNDLE_NAMESPACE}" 2>/dev/null || true

if [[ ! "${SA_WAIT_TIMEOUT_SECONDS}" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: SA_WAIT_TIMEOUT_SECONDS must be a positive integer, got: ${SA_WAIT_TIMEOUT_SECONDS}"
    exit 2
fi
if [[ ! "${SA_WAIT_POLL_INTERVAL_SECONDS}" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: SA_WAIT_POLL_INTERVAL_SECONDS must be a positive integer, got: ${SA_WAIT_POLL_INTERVAL_SECONDS}"
    exit 2
fi

SA_WAIT_MAX_ATTEMPTS=$(( (SA_WAIT_TIMEOUT_SECONDS + SA_WAIT_POLL_INTERVAL_SECONDS - 1) / SA_WAIT_POLL_INTERVAL_SECONDS ))
echo "Waiting for default serviceaccount in ${BUNDLE_NAMESPACE} (timeout=${SA_WAIT_TIMEOUT_SECONDS}s, poll=${SA_WAIT_POLL_INTERVAL_SECONDS}s)"
for _i in $(seq 1 "${SA_WAIT_MAX_ATTEMPTS}"); do
    if oc get serviceaccount default -n "${BUNDLE_NAMESPACE}" >/dev/null 2>&1; then
        break
    fi
    sleep "${SA_WAIT_POLL_INTERVAL_SECONDS}"
done
if ! oc get serviceaccount default -n "${BUNDLE_NAMESPACE}" >/dev/null 2>&1; then
    echo "ERROR: timed out waiting for serviceaccount/default in ${BUNDLE_NAMESPACE}"
    exit 1
fi

"${OPERATOR_SDK}" --security-context-config restricted -n "${BUNDLE_NAMESPACE}" \
    run bundle --timeout="${BUNDLE_RUN_TIMEOUT}" "${BUNDLE_IMG}" --install-mode="${OLM_INSTALL_MODE}" &
sdk_pid=$!

if [[ ! "${PATCH_TIMEOUT_SECONDS}" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: PATCH_TIMEOUT_SECONDS must be a positive integer, got: ${PATCH_TIMEOUT_SECONDS}"
    exit 2
fi

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
