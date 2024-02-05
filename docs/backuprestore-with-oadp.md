# Backup and restore with the Openshift API for Data Protection(OADP)

## Overview

The lifecycle Agent operator (LCA) provides functionality for backing up and restoring platform and application resources using OADP during image-based upgrades. During the upgrade stage, the LCA performs the following actions:

Before the cluster is rebooted to the new stateroot:

- Process the configmaps specified via `spec.oadpContent`.
- Apply all backup CRs wrapped in the configmaps. If any backup CR fails, the upgrade process is terminated.
- Export all restore CRs wrapped in the configmaps to the new stateroot.
- Export the live DataProtectionApplication(DPA) CR and the associated secrets used in the DPA to the new stateroot.

After the cluster is rebooted to the new stateroot:

- Restore the preserved secrets and DPA
- Apply all preverved restore CRs. If any restore CR fails, the upgrade process is terminated.

## Pre-Requisites

- A S3-compatible object storage must be set up and ensure that it's configured and accessible
- OADP operator must be installed on both target and seed SNOs
- OADP DataProtectionApplication CR and its secret must be installed on target SNOs

## LCA apply wave annotation

The annotation `lca.openshift.io/apply-wave` is supported in the backup or restore CR to define the order in which the backup or restore CRs should be applied by LCA. The value of the annotation should be a string number, for example:

```yaml
annotations:
  lca.openshift.io/apply-wave: "1"
```

If the annotation is provided in the backup or restore CRs, they will be applied in increasing order based on the annotation value. The LCA will move on the next group of CRs only after completing the previous group with the same wave number.
If no `lca.openshift.io/apply-wave` annotation is defined in the backup or restore CRs, which means no particular order is required, they will be applied all together.

## LCA apply label annotation

OADP backup doesn't support backing up specific CRs by object names, and the way to scope/filter backup specific resources is by using label selector.

This annotation provides a way for users to pass in the specific resources, and LCA will apply the label to those resources internally in order for the backup CR to only pick them up.

The resources could be passed using the `lca.openshift.io/apply-label` annotation. The value should be a list of comma
separated objects in `group/version/resource/name` format for cluster-scoped resources or
`group/version/resource/namespace/name` format for namespace-scoped resources, and it should be attached to the related
Backup CR. For example:

```yaml
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: acm-klusterlet
  namespace: openshift-adp
annotations:
  lca.openshift.io/apply-label: rbac.authorization.k8s.io/v1/clusterroles/klusterlet,apps/v1/deployments/open-cluster-management-agent/klusterlet
labels:
  velero.io/storage-location: default
spec:
  includeNamespace:
   - open-cluster-management-agent
  includeClusterScopedResources:
   - clusterroles
  includeNamespaceScopedResources:
   - deployments
```

By adding this annotation, LCA will limit the scope exclusively to those resources. LCA will parse the annotations and apply `lca.openshift.io/backup: true` label to those resources. LCA then adds this labelSelector when creating the backup CR:

```yaml
labelSelector:
  matchLabels:
    lca.openshift.io/backup: true
```

## Install OADP and configure OADP on target cluster via ZTP GitOps

 Install OADP via [GitOps ZTP pipeline](https://docs.openshift.com/container-platform/4.14/scalability_and_performance/ztp_far_edge/ztp-configuring-managed-clusters-policies.html).

### Prepare OADP install CRs

#### 1. Create a directory called `source-crs` in the same location where the `kustomization.yaml` file is located and push the following CRs to the `source-crs` directory

OadpSubscriptionNS.yaml

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-adp
  annotations:
    workload.openshift.io/allowed: management
    ran.openshift.io/ztp-deploy-wave: "2"
  labels:
    kubernetes.io/metadata.name: openshift-adp
```

OadpSubscriptionOperGroup.yaml

```yaml
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: openshift-adp
  namespace: openshift-adp
  annotations:
    ran.openshift.io/ztp-deploy-wave: "2"
spec:
  targetNamespaces:
  - openshift-adp
```

OadpSubscription.yaml

```yaml
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: openshift-adp
  namespace: openshift-adp
  annotations:
    ran.openshift.io/ztp-deploy-wave: "2"
spec:
  channel: stable-1.3
  name: redhat-oadp-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
  installPlanApproval: Manual
status:
  state: AtLatestKnown
```

OadpOperatorStatus.yaml

```yaml
# This CR verifies the installation/upgrade of the OADP
apiVersion: operators.coreos.com/v1
kind: Operator
metadata:
  name: redhat-oadp-operator.openshift-adp
  annotations:
    ran.openshift.io/ztp-deploy-wave: "2"
status:
  components:
    refs:
    - kind: Subscription
      namespace: openshift-adp
      conditions:
      - type: CatalogSourcesUnhealthy
        status: "False"
    - kind: InstallPlan
      namespace: openshift-adp
      conditions:
      - type: Installed
        status: "True"
    - kind: ClusterServiceVersion
      namespace: openshift-adp
      conditions:
      - type: Succeeded
        status: "True"
        reason: InstallSucceeded
```

tree ./policygentemplates

```console
├── kustomization.yaml
├── sno
│   ├── cnfdf37.yaml
│   ├── common-ranGen.yaml
│   ├── group-du-sno-ranGen.yaml
│   ├── group-du-sno-validator-ranGen.yaml
│   └── ns.yaml
├── source-crs
│   ├── OadpOperatorStatus.yaml
│   ├── OadpSubscriptionNS.yaml
│   ├── OadpSubscriptionOperGroup.yaml
│   ├── OadpSubscription.yaml
```

> [!IMPORTANT]
> The OADP must be installed in the `openshift-adp` namespace.

#### 2. Add the CRs to your common PGT. For example

```yaml
apiVersion: ran.openshift.io/v1
kind: PolicyGenTemplate
metadata:
  name: "common-latest"
  namespace: "ztp-common"
spec:
  bindingRules:
    # These policies will correspond to all clusters with this label:
    common: "true"
    du-profile: "latest"
  sourceFiles:
    - fileName: OadpSubscriptionNS.yaml
      policyName: "subscriptions-policy"
    - fileName: OadpSubscriptionOperGroup.yaml
      policyName: "subscriptions-policy"
    - fileName: OadpSubscription.yaml
      policyName: "subscriptions-policy"
    - fileName: OadpOperatorStatus.yaml
      policyName: "subscriptions-policy"
    ...
```

*TODO*: Add the OADP CRs to [ZTP source-crs](https://github.com/openshift-kni/cnf-features-deploy/tree/master/ztp/source-crs)

### Prepare DataProtectionApplication(DPA) CR and S3 secret

#### 1. Create the following CRs in your `source-crs` directory

DataProtectionApplication.yaml

```yaml
apiVersion: oadp.openshift.io/v1alpha1
kind: DataProtectionApplication
metadata:
  name: dataprotectionapplication
  namespace: openshift-adp
  annotations:
    ran.openshift.io/ztp-deploy-wave: "100"
spec:
  configuration:
    velero:
      defaultPlugins:
        - aws
        - openshift
      resourceTimeout: 10m
  backupLocations:
    - velero:
        config:
          profile: "default"
          region: minio
          s3Url: $placeholder_url
          insecureSkipTLSVerify: "true"
          s3ForcePathStyle: "true"
        provider: aws
        default: true
        credential:
          key: cloud
          name: cloud-credentials
        objectStorage:
          bucket: $placeholder_bucket
          prefix: velero
status:
  conditions:
  - reason: Complete
    status: "True"
    type: Reconciled
```

OadpSecret.yaml

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: cloud-credentials
  namespace: openshift-adp
  annotations:
    ran.openshift.io/ztp-deploy-wave: "100"
type: Opaque
```

OadpBackupStorageLocationStatus.yaml

```yaml
# This CR verifies the availability of backup storage locations created by OADP
apiVersion: velero.io/v1
kind: BackupStorageLocation
metadata:
  namespace: openshift-adp
  annotations:
    ran.openshift.io/ztp-deploy-wave: "100"
status:
  phase: Available
```

#### 2. Add the CRs to your site PGT with overrides. For example

```yaml
apiVersion: ran.openshift.io/v1
kind: PolicyGenTemplate
metadata:
  name: "cnfdf37"
  namespace: "ztp-site"
spec:
  bindingRules:
    sites: "cnfdf37"
    du-profile: "latest"
  mcp: "master"
  sourceFiles:
    ...
    - fileName: OadpSecret.yaml
      policyName: "config-policy"
      data:
        cloud: <replace-with-your-creds>
    - fileName: DataProtectionApplication.yaml
      policyName: "config-policy"
      spec:
        backupLocations:
          - velero:
              config:
                region: minio
                s3Url: <replace-with-your-s3-url>
              objectStorage:
                bucket: <replace-with-your-bucket-name>
    - fileName: OadpBackupStorageLocationStatus.yaml
      policyName: "config-policy"
```

Replace `<replace-with-your-creds>` with the encoded creds. For example,

```console
cat ./credentials-velero
[default]
aws_access_key_id=<AWS_ACCESS_KEY_ID>
aws_secret_access_key=<AWS_SECRET_ACCESS_KEY>

$ base64 -w0 ./credentials-velero
W2RlZmF1bHRdCmF3c19hY2Nlc3Nfa2V5X2lkPTxBV1NfQUNDRVNTX0tFWV9JRD4KYXdzX3NlY3JldF9hY2Nlc3Nfa2V5PTxBV1NfU0VDUkVUX0FDQ0VTU19LRVk+Cg==
```

> [!IMPORTANT]
>
> 1. The value of `spec.backupLocations[0].velero.config.profile` should match the name of the profile specified in the credentials-velero file.
> 2. The bucket specified in `spec.backupLocations[0].velero.objectStorage.bucket` must has been created in the S3 storage backend.
>

### OADP Configmap generation

#### 1. Create a local directory and store your backup and restore CRs in separate files. For example

backup_acm_klusterlet.yaml

```yaml
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: acm-klusterlet
  annotations:
    lca.openshift.io/apply-label: "apps/v1/deployments/open-cluster-management-agent/klusterlet,v1/secrets/open-cluster-management-agent/bootstrap-hub-kubeconfig,rbac.authorization.k8s.io/v1/clusterroles/klusterlet,v1/serviceaccounts/open-cluster-management-agent/klusterlet,rbac.authorization.k8s.io/v1/clusterroles/open-cluster-management:klusterlet-admin-aggregate-clusterrole,rbac.authorization.k8s.io/v1/clusterrolebindings/klusterlet,operator.open-cluster-management.io/v1/klusterlets/klusterlet,apiextensions.k8s.io/v1/customresourcedefinitions/klusterlets.operator.open-cluster-management.io,v1/secrets/open-cluster-management-agent/open-cluster-management-image-pull-credentials"
  labels:
    velero.io/storage-location: default
  namespace: openshift-adp
spec:
  includedNamespaces:
  - open-cluster-management-agent
  includedClusterScopedResources:
  - klusterlets.operator.open-cluster-management.io
  - clusterclaims.cluster.open-cluster-management.io
  - clusterroles
  - clusterrolebindings
  includedNamespaceScopedResources:
  - deployments
  - serviceaccounts
  - secrets
```

backup_localvolume.yaml

```yaml
apiVersion: velero.io/v1
kind: Backup
metadata:
  labels:
    velero.io/storage-location: default
  name: localvolume
  namespace: openshift-adp
spec:
  includedNamespaces:
  - openshift-local-storage
  includedNamespaceScopedResources:
  - localvolumes
  excludedClusterScopedResources:
  - Namespace
```

backup_app.yaml

```yaml
apiVersion: velero.io/v1
kind: Backup
metadata:
  labels:
    velero.io/storage-location: default
  name: small-app
  namespace: openshift-adp
spec:
  includedNamespaces:
  - test
  includedNamespaceScopedResources:
  - secrets
  - persistentvolumeclaims
  - deployments
  - statefulsets
  excludedClusterScopedResources:
  - persistentVolumes
```

restore_acm_klusterlet.yaml

```yaml
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: acm-klusterlet
  namespace: openshift-adp
  labels:
    velero.io/storage-location: default
  annotations:
    lca.openshift.io/apply-wave: "1"
spec:
  backupName:
    acm-klusterlet
```

restore_localvolumes.yaml

```yaml
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: localvolume
  namespace: openshift-adp
  labels:
    velero.io/storage-location: default
  annotations:
    lca.openshift.io/apply-wave: "2"
spec:
  backupName:
    localvolume
```

restore_app.yaml

```yaml
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: small-app
  namespace: openshift-adp
  labels:
    velero.io/storage-location: default
  annotations:
    lca.openshift.io/apply-wave: "3"
spec:
  backupName:
    small-app
```

> [!IMPORTANT]
>
> 1. The examples provided are just for reference. Create your own backup and restore CRs based on your needs.
> 2. The backup and restore CRs must be created in the same namespace where the OADP is installed which is `openshift-adp`.
> 3. The OADP configmaps should only contain backup and restore CRs which are in YAML format or multiple document YAML format. Other type of CRs are unknown to the LCA and will be ignored.
>

#### 2. Build a configmap to include all CRs

kustomization.yaml

```yaml
configMapGenerator:
- name: oadp-cm
  namespace: openshift-adp
  files:
  - backup_acm_klusterlet.yaml
  - backup_localvolume.yaml
  - backup_app.yaml
  - restore_acm_klusterlet.yaml
  - restore_localvolume.yaml
  - restore_app.yaml
generatorOptions:
  disableNameSuffixHash: true
```

```console
kustomize build ./ -o OadpCm.yaml
```

#### 3. Push the generated OadpCm.yaml to the same git directory `source-crs`

#### 4. Add the CR to your site PGT

```yaml
apiVersion: ran.openshift.io/v1
kind: PolicyGenTemplate
metadata:
  name: "cnfdf37"
  namespace: "ztp-site"
spec:
  bindingRules:
    sites: "cnfdf37"
    du-profile: "latest"
  mcp: "master"
  sourceFiles:
    ...
    - fileName: OadpCm.yaml
      policyName: "config-policy"
```

### Install OADP and its configuration via ClusterGroupUpgrade(CGU)

If you are installing OADP on day0, a `ClusterGroupUpgrade` CR will be auto-created to apply all the cluster configurations.
If you are installing OADP on day2, after the previous changes have been synced by ArgoCD and the policies have been updated, you will need to manually create a `ClusterGroupUpgrade`(CGU) to rollout the changes.

```yaml
apiVersion: ran.openshift.io/v1alpha1
kind: ClusterGroupUpgrade
metadata:
  name: oadp-install
spec:
  clusters:
  - cnfdf37
  enable: true
  managedPolicies:
  - common-subscriptions-policy
  - cnfdf37-config-policy
  remediationStrategy:
    maxConcurrency: 1
    timeout: 240
```

> [!NOTE]
> The ZTP GitOps method described above can be used to install OADP on the seed cluster as well. However, please note
> that you should not apply the DPA (Data Protection Application) and S3 secret on the seed cluster. These will be
> handled by the LCA during the upgrade process.

## Manually install OADP and configure OADP on target cluster

If you prefer to install and configure OADP manually, you can copy all the neccessary CRs provided in the section
**Install OADP and configure OADP on target cluster via ZTP GitOps**, remove the `ran.openshift.io/ztp-deploy-wave`
annotation from CRs and use them as a reference. Remove Make sure to adjust the configuration according to your specific
requirements.

## Update the IBU CR with OADP configmap

To enable backup and restore during the upgrade stage, you should update the IBU CR with the generated OADP configmap specified in the `spec.oadpContent` field.

```yaml
apiVersion: lca.openshift.io/v1alpha1
kind: ImageBasedUpgrade
metadata:
  name: upgrade
spec:
  ...
  oadpContent:
  - name: oadp-cm-8g2mm56c2f
    namespace: openshift-adp
```

## Monitoring backup or restore process

Monitor the LCA logs:

```console
oc logs -n openshift-lifecycle-agent -l app.kubernetes.io/component=lifecycle-agent -c  manager -f | grep BackupRestore
```

Watch the backup or restore CRs:

```console
watch -n 5 'oc get backups -n openshift-adp -o custom-columns=NAME:.metadata.name,Status:.status.phase,Reason:.status.failureReason'

watch -n 5 'oc get restores -n openshift-adp -o custom-columns=NAME:.metadata.name,Status:.status.phase,Reason:.status.failureReason'
```

## Debugging on a failed backup or restore CR

Install velero CLI by following the [installation guide](https://velero.io/docs/main/basic-install/#install-the-cli).

Describe the backup/resource CR with errors:

```console
velero describe backup -n openshift-adp backup-acm-klusterlet --details
velero describe restore -n openshift-adp restore-acm-klusterlet --details
```

Download the backed up resources to a local directory:

```console
velero backup download -n openshift-adp backup-acm-klusterlet -o ~/backup-acm-klusterlet.tar.gz
```
