#!/bin/bash
#
# Gather diagnostics after a failed OLM smoke test.
#
# Usage: olm-smoke-diagnostics.sh <namespace>

set -u +e
NAMESPACE="${1:?Usage: $0 <namespace>}"

section() { echo ""; echo "=== $* ==="; }

section "Operator Pods"
oc get pods -n "${NAMESPACE}" -o wide 2>/dev/null

section "Operator Logs"
oc logs -l "app.kubernetes.io/name=lifecycle-agent-operator" \
  -n "${NAMESPACE}" --tail=200 2>/dev/null

section "Events"
oc get events -n "${NAMESPACE}" --sort-by='.lastTimestamp' 2>/dev/null

section "IBU Status"
oc describe imagebasedupgrade upgrade 2>/dev/null

section "CSV Status"
oc get csv -n "${NAMESPACE}" -o yaml 2>/dev/null

section "CatalogSource Status"
oc get catalogsource -n "${NAMESPACE}" -o wide 2>/dev/null

section "InstallPlan Status"
oc get installplan -n "${NAMESPACE}" -o wide 2>/dev/null

section "Pods in OLM namespaces"
oc get pods -n openshift-operator-lifecycle-manager -o wide 2>/dev/null
oc get pods -n openshift-marketplace -o wide 2>/dev/null
