# Must-Gather

## Collect logs

We are collecting the following set of data with best-effort

- lca
  - inspect the namespace to get things such as pod logs (including precache) and other CRs such as configmaps  
  - `upgrade` and `seedimage` cluster-scoped CRs  
  - logs from seed-gen container and install service  
  - all CRs used for healthchecks
  - rpm-ostree status
  - recert-summary (recert-seed-restoration-summary.yaml + recert-seed-creation-summary.yaml + recert-summary.yaml)
  - unbooted stateroot's /var/log files
  - host's /etc
- Oadp  
  - CRs needed to install  
  - all CRs in OADP ns
  - all CRs defined by OADP  
- Sriov
  - all CRs defined by Sriov  

```shell
oc adm must-gather \
  --dest-dir=must-gather/tmp \
  --image=$(oc -n openshift-lifecycle-agent get deployment.apps/lifecycle-agent-controller-manager -o jsonpath='{.spec.template.spec.containers[?(@.name == "manager")].image}')
```

Additional OADP must-gather can be used by appending with

```shell
--image=quay.io/konveyor/oadp-must-gather:latest
```

Additional Sriov must-gather can be used by appending with

```shell
--image=quay.io/openshift/origin-must-gather
```

## Dev  

See [here](../DEVELOPING.md#must-gather)
