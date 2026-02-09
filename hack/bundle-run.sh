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
#   BUNDLE_NAMESPACE: namespace used for operator-sdk run bundle (default: openshift-lifecycle-agent)
#   CATALOGSOURCE_NAME: CatalogSource name created by operator-sdk (default: lifecycle-agent-catalog)
#   PATCH_TIMEOUT_SECONDS: how long to wait for CatalogSource/address to appear (default: 120)

set -euo pipefail
if [[ "${TRACE:-0}" == "1" ]]; then
    set -x
fi

: "${BUNDLE_NAMESPACE:=openshift-lifecycle-agent}"
: "${CATALOGSOURCE_NAME:=lifecycle-agent-catalog}"
: "${PATCH_TIMEOUT_SECONDS:=120}"

if [[ -z "${OPERATOR_SDK:-}" ]]; then
    echo "ERROR: OPERATOR_SDK is not set"
    exit 2
fi

if [[ -z "${BUNDLE_IMG:-}" ]]; then
    echo "ERROR: BUNDLE_IMG is not set"
    exit 2
fi

oc create ns "${BUNDLE_NAMESPACE}" 2>/dev/null || true

"${OPERATOR_SDK}" --security-context-config restricted -n "${BUNDLE_NAMESPACE}" run bundle "${BUNDLE_IMG}" &
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
