# Backup and restore with the Openshift API for Data Protection(OADP)

- [Backup and restore with the Openshift API for Data Protection(OADP)](#backup-and-restore-with-the-openshift-api-for-data-protectionoadp)
  - [Overview](#overview)
  - [Pre-Requisites](#pre-requisites)
  - [LCA apply wave annotation](#lca-apply-wave-annotation)
  - [LCA apply label annotation](#lca-apply-label-annotation)
  - [Install OADP and configure OADP on target cluster via ZTP GitOps](#install-oadp-and-configure-oadp-on-target-cluster-via-ztp-gitops)
    - [Prepare OADP install CRs](#prepare-oadp-install-crs)
    - [Prepare DataProtectionApplication(DPA) CR and S3 secret](#prepare-dataprotectionapplicationdpa-cr-and-s3-secret)
    - [Rollout OADP changes via ClusterGroupUpgrade(CGU)](#rollout-oadp-changes-via-clustergroupupgradecgu)
  - [Manually install OADP and configure OADP on target cluster](#manually-install-oadp-and-configure-oadp-on-target-cluster)
  - [OADP Configmap generation](#oadp-configmap-generation)
    - [Platform backup and restore CRs](#platform-backup-and-restore-crs)
    - [Application backup and restore CRs](#application-backup-and-restore-crs)
    - [Create OADP configmap with backup and restore CRs](#create-oadp-configmap-with-backup-and-restore-crs)
  - [Reference OADP configmap in IBU CR](#reference-oadp-configmap-in-ibu-cr)
  - [Monitoring backup or restore process](#monitoring-backup-or-restore-process)
  - [Debugging on a failed backup or restore CR](#debugging-on-a-failed-backup-or-restore-cr)

## Overview

The lifecycle Agent operator (LCA) provides functionality for backing up and restoring platform and application resources using OADP during image-based upgrades. During the upgrade stage, the LCA performs the following actions:

Before the cluster is rebooted to the new stateroot:

- Process the configmaps specified via `spec.oadpContent`.
- Cleanup any stale backup (with the same name) from the object storage. This cleanup is also done during the Prep stage.
- Apply all backup CRs wrapped in the configmaps (with the same `clusterID` label). If any backup CR fails, the upgrade process is terminated.
- Export all restore CRs wrapped in the configmaps to the new stateroot.
- Export the live DataProtectionApplication(DPA) CR and the associated secrets used in the DPA to the new stateroot.

After the cluster is rebooted to the new stateroot:

- Restore the preserved secrets and DPA
- Apply all preserved restore CRs (with the same `clusterID` label). If any restore CR fails, the upgrade process is terminated.

## Pre-Requisites

- A S3-compatible object storage must be set up and ensure that it's configured and accessible
- OADP operator must be installed on both target and seed SNOs
- OADP DataProtectionApplication CR and its secret must be installed on target SNOs only

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
  includedNamespaces:
   - open-cluster-management-agent
  includedClusterScopedResources:
   - clusterroles
  includedNamespaceScopedResources:
   - deployments
```

By adding this annotation, LCA will limit the scope exclusively to those resources. LCA will parse the annotations and apply `lca.openshift.io/backup: <backup-name>` label to those resources. LCA then adds this labelSelector when creating the backup CR:

```yaml
labelSelector:
  matchLabels:
    lca.openshift.io/backup: <backup-name>
```

> [!IMPORTANT]
> Please note that to use the `apply-label` annotation for backing up specific resources, the resources listed in the annotation should also be properly included in the spec.
> Additionally, if the `apply-label` annotation is used in the backup CR, only the resources listed in the annotation will be backed up, regardless of whether other resource types are specified in the spec or not.

## Install OADP and configure OADP on target cluster via ZTP GitOps

 Install OADP via [GitOps ZTP pipeline](https://docs.openshift.com/container-platform/4.14/scalability_and_performance/ztp_far_edge/ztp-configuring-managed-clusters-policies.html).

### Prepare OADP install CRs

#### 1. Create a directory called `custom-crs` in the `source-crs` directory. Ensure that the `source-crs` directory is located in the same location as `kustomization.yaml` file. Push the following CRs to the `source-crs/custom-crs` directory

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
  name: redhat-oadp-operator
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
  name: redhat-oadp-operator
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
└── source-crs
    └── custom-crs
        ├── OadpOperatorStatus.yaml
        ├── OadpSubscriptionNS.yaml
        ├── OadpSubscriptionOperGroup.yaml
        ├── OadpSubscription.yaml
```

> [!IMPORTANT]
> The OADP must be installed in the `openshift-adp` namespace.

#### 2. Add the install CRs to your common PGT. For example

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
    - fileName: custom-crs/OadpSubscriptionNS.yaml
      policyName: "subscriptions-policy"
    - fileName: custom-crs/OadpSubscriptionOperGroup.yaml
      policyName: "subscriptions-policy"
    - fileName: custom-crs/OadpSubscription.yaml
      policyName: "subscriptions-policy"
    - fileName: custom-crs/OadpOperatorStatus.yaml
      policyName: "subscriptions-policy"
...
```

*TODO*: Add the OADP CRs to [ZTP source-crs](https://github.com/openshift-kni/cnf-features-deploy/tree/master/ztp/source-crs)

### Prepare DataProtectionApplication(DPA) CR and S3 secret

1. Create the following config CRs in your `source-crs/custom-crs` directory

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
    restic:
      enable: false
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

> [!NOTE]
> In IBU scenarios, the PV contents are kept on the SNO disk and reused after pivot, hence the CSI Integration (i.e.,
> `.spec.configuration.restic`) should be always disabled in the `DataProtectionApplication`, see sample CR above.

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

#### 2. Add the config CRs to your site PGT with overrides. For example

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
    - fileName: custom-crs/OadpSecret.yaml
      policyName: "config-policy"
      data:
        cloud: <replace-with-your-creds>
    - fileName: custom-crs/DataProtectionApplication.yaml
      policyName: "config-policy"
      spec:
        backupLocations:
          - velero:
              config:
                profile: "default"   <1>
                region: minio
                s3Url: <replace-with-your-s3-url>
              objectStorage:
                bucket: <replace-with-your-bucket-name> <2>
                prefix: <replace-with-target-name>      <3>
    - fileName: custom-crs/OadpBackupStorageLocationStatus.yaml
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

> [!NOTE]
>
> 1. The value of `spec.backupLocations[0].velero.config.profile` should match the name of the profile specified in the credentials-velero file.
> 2. The bucket specified in `spec.backupLocations[0].velero.objectStorage.bucket` must have been created in the S3 storage backend.
> 3. The `spec.backupLocations[0].velero.objectStorage.prefix` defines the name of the sub directory that is auto-created in the bucket. The combination of bucket and prefix must be unique for each target cluster to avoid interference between them.

### Rollout OADP changes via ClusterGroupUpgrade(CGU)

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

If you prefer to install and configure OADP manually, you can copy all the necessary CRs provided in the section
**Install OADP and configure OADP on target cluster via ZTP GitOps**, remove the `ran.openshift.io/ztp-deploy-wave`
annotation from CRs and use them as a reference. Make sure to adjust the configuration according to your specific
requirements.

## OADP Configmap generation

The required backup and restore CRs over the upgrade are wrapped within configmaps. The OADP configmaps should only contain backup and restore CRs which are in YAML format or multiple document YAML format. Any other type of CRs are unrecognized to the LCA and will be disregarded.

### Platform backup and restore CRs

PlatformBackupRestore.yaml defines the ACM artifacts that must be backed up and restored over the upgrade. This is for RHACM environment only.

```yaml
apiVersion: velero.io/v1
kind: Backup
metadata:
  name: acm-klusterlet
  annotations:
    lca.openshift.io/apply-label: "apps/v1/deployments/open-cluster-management-agent/klusterlet,v1/secrets/open-cluster-management-agent/bootstrap-hub-kubeconfig,rbac.authorization.k8s.io/v1/clusterroles/klusterlet,v1/serviceaccounts/open-cluster-management-agent/klusterlet,scheduling.k8s.io/v1/priorityclasses/klusterlet-critical,rbac.authorization.k8s.io/v1/clusterroles/open-cluster-management:klusterlet-admin-aggregate-clusterrole,rbac.authorization.k8s.io/v1/clusterrolebindings/klusterlet,operator.open-cluster-management.io/v1/klusterlets/klusterlet,apiextensions.k8s.io/v1/customresourcedefinitions/klusterlets.operator.open-cluster-management.io,v1/secrets/open-cluster-management-agent/open-cluster-management-image-pull-credentials"
  labels:
    velero.io/storage-location: default
  namespace: openshift-adp
spec:
  includedNamespaces:
  - open-cluster-management-agent
  includedClusterScopedResources:
  - klusterlets.operator.open-cluster-management.io
  - clusterroles.rbac.authorization.k8s.io
  - clusterrolebindings.rbac.authorization.k8s.io
  - priorityclasses.scheduling.k8s.io
  includedNamespaceScopedResources:
  - deployments
  - serviceaccounts
  - secrets
---
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

> [!IMPORTANT]
>
> 1. Depending on Red Hat's ACM configuration the `v1/secrets/open-cluster-management-agent/open-cluster-management-image-pull-credentials` object must be required to back up or not. Please, make sure if your multiclusterhub CR has `.spec.imagePullSecret`
> defined and the secret exists on the open-cluster-management-agent namespace in your hub cluster. If it does not exist, you can safely remove it from the `apply-label` annotation.
> 2. If ACM is below 2.10, and MCE is below 2.5.0, the `scheduling.k8s.io/v1/priorityclasses/klusterlet-critical` must be excluded from `lca.openshift.io/apply-label`, along with `priorityclasses.scheduling.k8s.io` from `spec.includedClusterScopedResources`.

There are two local storage implementations for SNO. One is provided by Local Storage Operator (LSO), and the other is provided by Logical Volume Manager Storage (LVMS). The persistent volumes must be provided by either LSO or LVMS on a given target cluster for IBU, not both.
When LSO is used to create persistent storage in the target cluster, LCA automatically backs up all the `LocalVolumes` CRs along with their associated `StorageClasses` CRs at the pre-pivot and restores them at the post-pivot upgrade. No backup and restore CRs are needed.

However, when LVMS is used, the following `PlatformBackupRestoreLvms.yaml` defining the LVMS artifacts must be added in the OADP configmap. These objects will ensure the proper / expected functioning of this feature when using LVMS.

```yaml
apiVersion: velero.io/v1
kind: Backup
metadata:
  labels:
    velero.io/storage-location: default
  name: lvmcluster
  namespace: openshift-adp
spec:
  includedNamespaces:
    - openshift-storage
  includedNamespaceScopedResources:
    - lvmclusters
    - lvmvolumegroups
    - lvmvolumegroupnodestatuses
---
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: lvmcluster
  namespace: openshift-adp
  labels:
    velero.io/storage-location: default
  annotations:
    lca.openshift.io/apply-wave: "2"
spec:
  backupName:
    lvmcluster
```

> [!IMPORTANT]
> Mind that `apply-wave` annotation for the above platform restore objects should be numerically lower than the app's one, in this way
> when the app resources are restored, the platform will be ready and the `LVMCluster` operand will be already up and running in the cluster.

### Application backup and restore CRs

A separate backup and restore CR must be created to scope the backup to the specific cluster scoped resources created by the application. Here is an example:

ApplicationClusterScopedBackupRestore.yaml

```yaml
apiVersion: velero.io/v1
kind: Backup
metadata:
  annotations:
    lca.openshift.io/apply-label: "apiextensions.k8s.io/v1/customresourcedefinitions/test.example.com,security.openshift.io/v1/securitycontextconstraints/test,rbac.authorization.k8s.io/v1/clusterroles/test-role,rbac.authorization.k8s.io/v1/clusterrolebindings/system:openshift:scc:test" <1>
  name: backup-app-cluster-resources
  labels:
    velero.io/storage-location: default
  namespace: openshift-adp
spec:
  includedClusterScopedResources:
  - customresourcedefinitions
  - securitycontextconstraints
  - clusterrolebindings
  - clusterroles
  excludedClusterScopedResources:
  - Namespace
---
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: test-app-cluster-resources
  namespace: openshift-adp
  labels:
    velero.io/storage-location: default
  annotations:
    lca.openshift.io/apply-wave: "3"
spec:
  backupName:
    test-app-cluster-resources
```

> [!NOTE]
>
> 1. As mentioned in above section **LCA apply label annotation**, the annotation `lca.openshift.io/apply-label` offers a method to back up specific resources exclusively. You are required to replace the example resource names in the `lca.openshift.io/apply-label` with your actual resources.

When LSO is used to create persistent volumes, the `persistentVolumes` must be excluded in the application backup CR. Here is an example:

ApplicationBackupRestoreLso.yaml

```yaml
apiVersion: velero.io/v1
kind: Backup
metadata:
  labels:
    velero.io/storage-location: default
  name: test-app
  namespace: openshift-adp
spec:
  includedNamespaces:
  - test
  includedNamespaceScopedResources:
  - secrets
  - persistentvolumeclaims
  - deployments
  - statefulsets
  - configmaps
  - cronjobs
  - services
  - job
  - poddisruptionbudgets
  - <application custom resources>   <1>
  excludedClusterScopedResources:
  - persistentVolumes
---
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: test-app
  namespace: openshift-adp
  labels:
    velero.io/storage-location: default
  annotations:
    lca.openshift.io/apply-wave: "4" 
spec:
  backupName:
    test-app
```

> [!NOTE]
>
> 1. Custom resources created by the application if any

When LVMS is used to create persistent volumes, ensure that the following required fields are included to guarantee the preservation of each PV content during IBU. Here is an example:

ApplicationBackupRestoreLvms.yaml

```yaml
apiVersion: velero.io/v1
kind: Backup
metadata:
  labels:
    velero.io/storage-location: default
  name: test-app
  namespace: openshift-adp
spec:
  includedNamespaces:
  - test
  includedNamespaceScopedResources:
  - secrets
  - persistentvolumeclaims
  - deployments
  - statefulsets
  - configmaps
  - cronjobs
  - services
  - job
  - poddisruptionbudgets
  - <application custom resources>  <1>
  includedClusterScopedResources:
  - persistentVolumes              # <- required field
  - logicalvolumes.topolvm.io      # <- required field
---
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: test-app
  namespace: openshift-adp
  labels:
    velero.io/storage-location: default
  annotations:
    lca.openshift.io/apply-wave: "4"
spec:
  backupName:
    test-app
  restorePVs: true               # <- required field
  restoreStatus:                 # <- required field
    includedResources:           # <- required field
    - logicalvolumes             # <- required field
```

> [!NOTE]
>
> 1. Custom resources created by the application if any
> 2. If volume snapshots are used, ensure to include `volumesnapshotcontents` in the `includedClusterScopedResources` as well

> [!IMPORTANT]
>
> 1. Adjust the examples for application backup and restore CRs based on your application.
> 2. The backup and restore CRs must be created in the same namespace where the OADP is installed which is `openshift-adp`.

### Create OADP configmap with backup and restore CRs

#### Use ZTP GitOps

The platform backup and restore CRs [PlatformBackupRestore.yaml](https://github.com/openshift-kni/cnf-features-deploy/blob/master/ztp/source-crs/ibu/PlatformBackupRestore.yaml) and
[PlatformBackupRestoreLvms.yaml](https://github.com/openshift-kni/cnf-features-deploy/blob/master/ztp/source-crs/ibu/PlatformBackupRestoreLvms.yaml) are provided in the ZTP container.
Follow the [instruction](https://github.com/openshift-kni/cnf-features-deploy/tree/master/ztp/gitops-subscriptions/argocd/example/image-based-upgrades#generating-the-oadp-configmap-and-policies) to create OADP configmap on target clusters using ACM policy.

#### Non-ZTP GitOps

Create OADP configmap with `oc create configmap` on the target cluster with the above necessary backup and restore yamls, for example:

```console
$ oc create configmap oadp-cm  -n openshift-adp \
--from-file=PlatformBackupRestore.yaml=PlatformBackupRestore.yaml \
--from-file=ApplicationClusterScopedBackupRestore.yaml \
--from-file=ApplicationBackupRestoreLso.yaml
```

## Reference OADP configmap in IBU CR

To enable backup and restore during the upgrade stage, you should update the IBU CR with the generated OADP configmap specified in the `spec.oadpContent` field.

```yaml
apiVersion: lca.openshift.io/v1
kind: ImageBasedUpgrade
metadata:
  name: upgrade
spec:
  ...
  oadpContent:
  - name: oadp-cm
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

The Velero CLI is accessible in the Velero pod for further debugging. Alternatively, you can install Velero CLI locally by following the [installation guide](https://velero.io/docs/main/basic-install/#install-the-cli).

Describe the backup/resource CR with errors:

```console
oc exec -n openshift-adp velero-7c87d58c7b-sw6fc -c velero -- ./velero describe backup -n openshift-adp backup-acm-klusterlet --details
oc exec -n openshift-adp velero-7c87d58c7b-sw6fc -c velero -- ./velero describe restore -n openshift-adp restore-acm-klusterlet --details
```

Download the backed up resources to a local directory:

```console
oc exec -n openshift-adp velero-7c87d58c7b-sw6fc -c velero -- ./velero backup download -n openshift-adp backup-acm-klusterlet -o ~/backup-acm-klusterlet.tar.gz
```
