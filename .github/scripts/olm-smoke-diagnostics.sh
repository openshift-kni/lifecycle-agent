#!/bin/bash
#
# Gather diagnostics after a failed OLM smoke test.

set -u +e
NAMESPACE="${1:?Usage: $0 <namespace>}"
OPERATOR_LABEL="app.kubernetes.io/name=lifecycle-agent-operator"

section() { echo ""; echo "=== $* ==="; }

if ! command -v kubectl >/dev/null 2>&1; then
    echo "kubectl not available; skipping cluster diagnostics"
    exit 0
fi

section "Operator Pods"
kubectl get pods -n "${NAMESPACE}" -o wide

section "Operator Logs"
kubectl logs -l "${OPERATOR_LABEL}" \
    -n "${NAMESPACE}" --tail=200

section "Events"
kubectl get events -n "${NAMESPACE}" --sort-by='.lastTimestamp'

section "CSV Status"
kubectl get csv -n "${NAMESPACE}" -o yaml

section "CatalogSource Status"
kubectl get catalogsource -n "${NAMESPACE}" -o wide

section "InstallPlan Status"
kubectl get installplan -n "${NAMESPACE}" -o wide

section "OLM pods"
kubectl get pods -n olm -o wide
kubectl get pods -n operators -o wide

exit 0
