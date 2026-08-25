#!/bin/bash
#
# Verify OLM ingested the lifecycle-agent bundle on vanilla Kubernetes.
# CRDs, RBAC, and CEL are asserted; CSV Succeeded and a Ready pod are not
# expected because the operator exits without OpenShift APIServer.

set -euo pipefail

NAMESPACE="${1:?Usage: $0 <namespace>}"

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; exit 1; }
section() { echo ""; echo "=== $* ==="; }

# Reject a wrong-name singleton and require the CEL message in the apply error.
expect_cel_reject() {
    local kind="$1"
    local message="$2"
    local out
    echo "Attempting to create ${kind} with wrong name (should fail)..."
    if out=$(kubectl apply -f - 2>&1); then
        fail "${kind} with wrong name was accepted (CEL validation not enforced)"
    fi
    echo "${out}"
    echo "${out}" | grep -Fq "${message}" || fail "${kind} rejected without CEL message '${message}'"
    pass "${kind} wrong-name correctly rejected"
}

section "Verifying CSV exists"
CSV_JSON=$(kubectl get csv -n "${NAMESPACE}" -o json)
CSV_COUNT=$(echo "${CSV_JSON}" | jq '.items | length')
[ "${CSV_COUNT}" -ge 1 ] || fail "expected at least 1 CSV, found ${CSV_COUNT}"
PHASE=$(echo "${CSV_JSON}" | jq -r '.items[0].status.phase')
CSV_NAME=$(echo "${CSV_JSON}" | jq -r '.items[0].metadata.name')
echo "CSV ${CSV_NAME}: phase=${PHASE}"
pass "CSV exists (phase=${PHASE}; Succeeded not required on Kind)"

section "Verifying CRDs"
for CRD in \
    imagebasedupgrades.lca.openshift.io \
    seedgenerators.lca.openshift.io \
    ipconfigs.lca.openshift.io; do
    kubectl get crd "${CRD}" > /dev/null
    pass "CRD ${CRD} exists"
done

section "Verifying RBAC"
kubectl get sa lifecycle-agent-controller-manager -n "${NAMESPACE}" > /dev/null
pass "ServiceAccount exists"

# Manager RBAC is inlined in the CSV. Extra bundle objects (metrics ClusterRole,
# metrics Service) are not applied while the CSV is Pending on Kind.
echo "${CSV_JSON}" | jq -e \
    '.items[0].spec.install.spec.clusterPermissions | length > 0' \
    > /dev/null || fail "CSV has no clusterPermissions"
pass "CSV clusterPermissions present"

for res in imagebasedupgrades seedgenerators ipconfigs; do
    echo "${CSV_JSON}" | jq -e --arg r "${res}" \
        '[.items[0].spec.install.spec.clusterPermissions[].rules[].resources[]?] | any(. == $r)' \
        > /dev/null || fail "CSV clusterPermissions missing resource ${res}"
    pass "CSV clusterPermissions include ${res}"
done

section "Verifying CEL singleton name enforcement"

expect_cel_reject "IBU" "ibu is a singleton, metadata.name must be 'upgrade'" <<'EOF'
apiVersion: lca.openshift.io/v1
kind: ImageBasedUpgrade
metadata:
  name: wrong-name
spec:
  stage: Idle
EOF

expect_cel_reject "SeedGenerator" "seedgen is a singleton, metadata.name must be 'seedimage'" <<'EOF'
apiVersion: lca.openshift.io/v1
kind: SeedGenerator
metadata:
  name: wrong-name
spec:
  seedImage: quay.io/example/seed:latest
  recertImage: quay.io/example/recert:latest
EOF

expect_cel_reject "IPConfig" "ipconfig is a singleton, metadata.name must be 'ipconfig'" <<'EOF'
apiVersion: lca.openshift.io/v1
kind: IPConfig
metadata:
  name: wrong-name
spec:
  stage: Idle
EOF

section "All smoke test checks passed"
