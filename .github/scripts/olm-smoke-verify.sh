#!/bin/bash
#
# Verify the lifecycle-agent operator after OLM installation.
# Checks CSV phase, deployment health, CRDs, RBAC, singletons, and CEL validation.
#
# Usage: olm-smoke-verify.sh <namespace>

set -euo pipefail

NAMESPACE="${1:?Usage: $0 <namespace>}"
OPERATOR_LABEL="app.kubernetes.io/name=lifecycle-agent-operator"

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; exit 1; }
section() { echo ""; echo "=== $* ==="; }

section "Verifying CSV phase"
PHASE=$(oc get csv -n "${NAMESPACE}" -o jsonpath='{.items[0].status.phase}')
CSV_NAME=$(oc get csv -n "${NAMESPACE}" -o jsonpath='{.items[0].metadata.name}')
echo "CSV ${CSV_NAME}: phase=${PHASE}"
[ "${PHASE}" = "Succeeded" ] || fail "CSV phase is '${PHASE}', expected 'Succeeded'"
pass "CSV phase is Succeeded"

section "Verifying operator deployment"
oc rollout status deployment -l "${OPERATOR_LABEL}" \
  -n "${NAMESPACE}" --timeout=120s
pass "Deployment rollout complete"

section "Verifying operator pod health"
POD_JSON=$(oc get pod -l "${OPERATOR_LABEL}" \
  -n "${NAMESPACE}" -o json | jq '.items[0]')
POD=$(echo "${POD_JSON}" | jq -r '.metadata.name')
STATUS=$(echo "${POD_JSON}" | jq -r '.status.phase')
READY=$(echo "${POD_JSON}" | jq -r '.status.conditions[] | select(.type=="Ready") | .status')
RESTARTS=$(echo "${POD_JSON}" | jq -r '.status.containerStatuses[0].restartCount')
echo "Pod ${POD}: phase=${STATUS}, ready=${READY}, restarts=${RESTARTS}"
[ "${STATUS}" = "Running" ] || fail "Pod phase is '${STATUS}', expected 'Running'"
[ "${READY}" = "True" ] || fail "Pod readiness is '${READY}', expected 'True'"
[ "${RESTARTS}" = "0" ] || fail "Pod has ${RESTARTS} restart(s), expected 0"
pass "Operator pod healthy (Running, Ready, 0 restarts)"

section "Verifying CRDs"
for CRD in imagebasedupgrades.lca.openshift.io \
           seedgenerators.lca.openshift.io \
           ipconfigs.lca.openshift.io; do
  oc get crd "${CRD}" > /dev/null
  pass "CRD ${CRD} exists"
done

section "Verifying RBAC and metrics"
oc get sa lifecycle-agent-controller-manager -n "${NAMESPACE}" > /dev/null
pass "ServiceAccount exists"

for ROLE in lifecycle-agent-manager-role \
            lifecycle-agent-imagebasedupgrade-editor-role \
            lifecycle-agent-imagebasedupgrade-viewer-role \
            lifecycle-agent-metrics-reader; do
  oc get clusterrole "${ROLE}" > /dev/null
  pass "ClusterRole ${ROLE} exists"
done

oc get clusterrolebinding lifecycle-agent-manager-rolebinding > /dev/null
pass "ClusterRoleBinding exists"

oc get service lifecycle-agent-controller-manager-metrics-service \
  -n "${NAMESPACE}" > /dev/null
pass "Metrics Service exists"

section "Verifying auto-created singletons"
oc wait imagebasedupgrade upgrade --for=condition=Idle=True --timeout=120s
pass "IBU singleton 'upgrade' exists and is Idle"

oc get seedgenerator seedimage > /dev/null
pass "SeedGenerator singleton 'seedimage' exists"

oc get ipconfig ipconfig > /dev/null
pass "IPConfig singleton 'ipconfig' exists"

section "Verifying CEL singleton name enforcement"

echo "Attempting to create IBU with wrong name (should fail)..."
if oc apply -f - 2>&1 <<'EOF'; then
apiVersion: lca.openshift.io/v1
kind: ImageBasedUpgrade
metadata:
  name: wrong-name
spec:
  stage: Idle
EOF
  fail "IBU with wrong name was accepted (CEL validation not enforced)"
else
  pass "IBU wrong-name correctly rejected"
fi

echo "Attempting to create SeedGenerator with wrong name (should fail)..."
if oc apply -f - 2>&1 <<'EOF'; then
apiVersion: lca.openshift.io/v1
kind: SeedGenerator
metadata:
  name: wrong-name
spec:
  seedImage: quay.io/example/seed:latest
  recertImage: quay.io/example/recert:latest
EOF
  fail "SeedGenerator with wrong name was accepted (CEL validation not enforced)"
else
  pass "SeedGenerator wrong-name correctly rejected"
fi

section "All smoke test checks passed"
