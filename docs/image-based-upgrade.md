# Image Based Upgrade

- [Image Based Upgrade](#image-based-upgrade)
  - [Overview](#overview)
  - [Handling Site Specific Artifacts](#handling-site-specific-artifacts)
    - [Backup and Restore](#backup-and-restore)
    - [Extra Manifests](#extra-manifests)
  - [Target SNO Prerequisites](#target-sno-prerequisites)
  - [ImageBasedUpgrade CR](#imagebasedupgrade-cr)
    - [Stage transitions](#stage-transitions)
  - [Image Based Upgrade Walkthrough](#image-based-upgrade-walkthrough)
    - [Disable auto importing of managed cluster](#disable-auto-importing-of-managed-cluster)
    - [Success Path](#success-path)
      - [Starting the Prep stage](#starting-the-prep-stage)
      - [Starting the Upgrade stage](#starting-the-upgrade-stage)
    - [Rollback after Pivot](#rollback-after-pivot)
    - [Automatic Rollback on Upgrade Failure](#automatic-rollback-on-upgrade-failure)
      - [Configuring Automatic Rollback](#configuring-automatic-rollback)
    - [Finalizing or Aborting](#finalizing-or-aborting)
      - [Finalize or Abort failure](#finalize-or-abort-failure)
    - [Monitoring Progress](#monitoring-progress)

## Overview

Image Based Upgrade (IBU) is a feature that allows a SNO to be upgraded via a seed image. The seed is an OCI image
generated from a SNO system installed with the target OCP version with most of the configuration for the target cluster
profile baked in. The seed image is installed onto a target SNO as a new ostree stateroot. Prior to booting this new
stateroot, application artifacts are backed up. After pivoting into the new release, the cluster is reconfigured with
the original cluster configuration, re-registered with ACM if applicable, and restored with the backed up application
artifacts. This results in a system functionally identical running a new release of OCP. The dual state root mechanism
facilitates easy rollbacks in the event of an upgrade failure.

The Lifecycle Agent provides orchestration of an IBU on the target SNO via the `ImageBasedUpgrade` CR. An upgrade is performed by patching the IBU CR through a series of stages.

- Idle
  - This is the initial stage, as well as the end stage of an upgrade flow (whether successful or not).
  - The IBU CR is always auto created with the idle stage when the LCA is installed the first time.
  - When transitioning from other stages back to Idle, LCA performs different cleanup logic specific to the stage, making sure the node is ready for another image based upgrade.

- Prep
  - This stage can only be set when the IBU is idle.
  - During this stage, LCA does as much preparation as possible for the upgrade without impacting the current running version. This includes downloading the seed image, unpacking it as a new ostree stateroot and pulling all images specified by the image list built into the seed image, refer to [precache-plugin](precache-plugin.md)

- Upgrade
  - This stage can only be set if the prep stage completed successfully.
  - This is where the actual upgrade happens. It consists of three main steps: pre-pivot, pivot and post-pivot.
    - Before pivoting the new state root, LCA collects the required cluster specific info/artifacts and store them in
      the new state root. It also performs [backups](backup-and-restore). Additionally, it exports CRs specified by the
`extraManifests` field in the IBU spec as well as the CRs described in the ZTP policies bound to the cluster for the
target OCP version.
    - Once pre pivot step is completed, LCA makes the new state root the default and reboots the node.
    - After booting from the new state root, LCA reconfigures the cluster so it looks like the original cluster by applying cluster specific info saved in the pre-pivot step (ref recert?). It and applies all saved CRs, and restores the [backups](backup-and-restore).

- Rollback
  - This is an optional stage. It can be set if upgrade has gone beyond the pivot step, whether it has completed, failed or still in progress. The LCA performs the rollback by setting the original state root as default and rebooting the node.

TODO Insert the state transition diagram

## Handling Site Specific Artifacts

In order to optimize the downtime of IBU, the intent is to apply as many artifacts as possible to the seed image.
The applications are considered site specific and are not part of this seed image.
In addition, there are platform artifacts that are site and OCP version specific so cannot simply be backed up and restored.

The LCA provides mechanisms to update the target cluster to address the above scenarios.

### Backup and Restore

This is mainly intended for the applications running on the SNO whose artifacts will not change as a result of the OCP version change.
A requirement of IBU is that the application version does not change over the upgrade.

It will also be used for a small number of platform artifacts that will not change over the upgrade (ex. ACM klusterlet).

The OADP operator is used to implement the backup and restore functionality. A configmap(s) is applied to the cluster and
specified by the `oadpContent` field in the [IBU CR](#imagebasedupgrade-cr). This configmap will contain a set of OADP
backup and restore CR. Refer to [backuprestore-with-oadp](backuprestore-with-oadp.md).

### Extra Manifests

The Life Cycle Agent provides a mechanism to apply a set of extra manifests after booting the new OCP version.
This is generally intended for platform configuration that is site specific and could change over OCP releases. It would also be used for new manifests that are only relevant in the new release.

There are two implementations of extra manifests:

- If the target cluster is integrated with ZTP GitOps, the site specific manifests can be automatically extracted from the policies by the operator during the upgrade stage.
Manifests defined in the policies that have the `ran.openshift.io/ztp-deploy-wave` annotation and labeled with `lca.openshift.io/target-ocp-version: "4.y.x"` or `lca.openshift.io/target-ocp-version: "4.y"` will be extracted and applied after rebooting to the new version.
For example,

  ```yaml
  kind: Policy
  apiVersion: policy.open-cluster-management.io/v1
  metadata:
    name: example-policy
    annotations:
      ran.openshift.io/ztp-deploy-wave: "1"
  spec:
    policy-templates:
    - objectDefinition:
        apiVersion: policy.open-cluster-management.io/v1
        kind: ConfigurationPolicy
        metadata:
          name: example-policy-config
        spec:
          object-templates:
          - objectDefinition:
              apiVersion: operators.coreos.com/v1alpha1
              kind: CatalogSource
              metadata:
                name: redhat-operators-new
                namespace: openshift-marketplace
                labels:
                  lca.openshift.io/target-ocp-version: "4.16"
              spec:
                displayName: Red Hat Operators Catalog
                image: registry.redhat.io/redhat/redhat-operator-index:v4.16
                publisher: Red Hat
                sourceType: grpc
                updateStrategy:
                  registryPoll:
                    interval: 1h
  ...
  ```

  If the annotation `lca.openshift.io/target-ocp-version-manifest-count` is specified in the IBU CR, LCA will verify that the number of manifests labeled with `lca.openshift.io/target-ocp-version` extracted from policies matches the count provided in the annotation during the prep and upgrade stages.
  For example,

  ```yaml
  apiVersion: lca.openshift.io/v1alpha1
  kind: ImageBasedUpgrade
  metadata:
    annotations:
      lca.openshift.io/target-ocp-version-manifest-count: "5"
    name: upgrade
  ```

- If the target cluster is not integrated with ZTP GitOps the extra manifests can be provided via configmap(s) applied to the cluster. These configmap(s) specified by the
`extraManifests` field in the [IBU CR](#imagebasedupgrade-cr). After rebooting to the new version, these extra manifests are applied.

  The annotation `lca.openshift.io/apply-wave` is supported for defining the manifest applying order. The value of the annotation should be a string number, for example:

  ```yaml
  annotations:
    lca.openshift.io/apply-wave: "1"
  ```

  If the annotation is provided in the manifests, they will be applied in increasing order based on the annotation value. Manifests without the annotation will be applied last.

## Target SNO Prerequisites

The target SNO has the following prerequisites:

- A compatible seed image has been generated [Seed Image Generation](seed-image-generation.md) and pushed to a registry accessible from the SNO
- The CPU topology must align with the seed SNO.
  - Same number of cores.
  - Same performance tuning (i.e., reserved CPUs).
- LCA operator must be deployed, version must be compatible with the seed.
- The OADP operator is installed along with a DataProtectionApplication CR. OADP has connectivity to a S3 backend
- The target cluster must have a dedicated partition configured for `/var/lib/containers`

## ImageBasedUpgrade CR

The spec fields include:

- stage: defines the desired stage for the IBU (Idle, Prep, Upgrade or Rollback)
- seedImageRef: defines the target OCP version, the seed image to be used and the secret required for accessing the image
- oadpContent: defines the list of config maps where the OADP backup / restore CRs are stored. This is optional
- extraManifests: defines the list of config maps where the additional CRs to be re-applied are stored
- autoRollbackOnFailure: configures the auto-rollback feature for upgrade failure, which is enabled by default
  - initMonitorTimeoutSeconds: set the LCA Init Monitor timeout duration, in seconds. The default value is 1800 (30 minutes).
    Setting a value less than or equal to 0 will use the default
  - See [Configuring Automatic Rollback](#configuring-automatic-rollback) for more.

The IBU CR status includes a list of conditions that indicates the progress of each stage:

- Idle
- PrepInProgress
- PrepCompleted
- UpgradeInProgress
- UpgradeCompleted
- RollbackInProgress
- RollbackCompleted

When LCA is installed, the IBU CR is auto-created:

```console
apiVersion: lca.openshift.io/v1alpha1
kind: ImageBasedUpgrade
metadata:
  creationTimestamp: "2024-04-19T19:20:04Z"
  generation: 1
  name: upgrade
  resourceVersion: "333525"
  uid: 511d9069-ce61-4029-bb7f-76832d5af356
spec:
  autoRollbackOnFailure: {}
  seedImageRef: {}
  stage: Idle
status:
  conditions:
  - lastTransitionTime: "2024-04-19T19:20:04Z"
    message: Idle
    observedGeneration: 1
    reason: Idle
    status: "True"
    type: Idle
  observedGeneration: 1
  validNextStages:
  - Prep
```

### Stage transitions

LCA will reject the stage transition if it is an invalid transition.

This table presents a comprehensive list of conditions and valid next stages based on the current stage.

| Current Stage | Condition          | Status | Reason         | After Pivot | Valid Next Stages |
|---------------|--------------------|--------|----------------|-------------|-------------------|
| `Idle`        | Idle               | True   | Idle           | N/A         | Prep              |
|               |                    | False  | Aborting       | False       | N/A               |
|               |                    | False  | Finalizing     | True        | N/A               |
|               |                    | False  | AbortFailed    | False       | N/A               |
|               |                    | False  | FinalizeFailed | True        | N/A               |
| `Prep`        | PrepInProgress     | True   | InProgress     | False       | Idle              |
|               | PrepCompleted      | False  | Failed         | False       | Idle              |
|               |                    | True   | Completed      | False       | Idle, Upgrade     |
| `Upgrade`     | UpgradeInProgress  | True   | InProgress     | False       | Idle              |
|               |                    | True   | InProgress     | True        | Rollback          |
|               | UpgradeCompleted   | False  | Failed         | False       | Idle              |
|               |                    | False  | Failed         | True        | Rollback          |
|               |                    | True   | Completed      | True        | Idle, Rollback    |
| `Rollback`    | RollbackInProgress | True   | InProgress     | True        | N/A               |
|               | RollbackCompleted  | False  | Failed         | True        | N/A               |
|               |                    | True   | Completed      | True        | Idle              |

If unexpected rejection occurs that block any spec changes, the annotation `lca.openshift.io/trigger-reconcile` serves as a backdoor to trigger the reconciliation for LCA to rectify the situation by adding or updating the annotation in the IBU CR.

## Image Based Upgrade Walkthrough

The Lifecycle Agent provides orchestration of the image based upgrade, triggered by patching the `ImageBasedUpgrade` CR through a series of stages.

### Disable auto importing of managed cluster

Before starting the upgrade, apply the annotation "import.open-cluster-management.io/disable-auto-import" to the managed cluster on the hub.This action is taken to disable automatic importing of the managed cluster during the upgrade stage until the cluster is prepared for it.
The annotation should be removed after the upgrade is completed.

```console
# Add the annotation to target managedcluster
oc annotate managedcluster <target-cluster-name> import.open-cluster-management.io/disable-auto-import=true

# Remove the annotation
oc annotate managedcluster <target-cluster-name> import.open-cluster-management.io/disable-auto-import-
```

### Success Path

The success path upgrade will progress through the following stages:

Idle -> Prep -> Upgrade -> Idle

#### Starting the Prep stage

The administrator patches the imagebasedupgrade CR:

- Set the stage to "Prep"
- Update the seedImageRef to the desired version and the seed image path
- Optional: Provide a reference to a configmap(s) of OADP backup and restore CRs, oadpContent
- Optional: Provide a reference to a configmap(s) of extra manifests, extraManifests

```console
oc patch imagebasedupgrade upgrade -n openshift-lifecycle-agent --type='json' -p='[
  {
    "op": "replace",
    "path": "/spec",
    "value": {
      "oadpContent": [{"name": "oadp-cm", "namespace": "openshift-adp"}],
      "extraManifests": [{"name": "test-extramanifests", "namespace": "openshift-lifecycle-agent"}],
      "stage": "Prep",
      "seedImageRef": {
        "version": "4.15.0,
        "image": "quay.io/user/seedimage:lca-test-seed-v1",
        "pullSecretRef": {"name": "seed-pull-secret"}
      }
    }
  }
]'
```

The "Prep" stage will:

- Pull the seed image
- Perform the following validations:
  - If the oadpContent is populated, validate that the specified configmap has been applied and is valid
  - If the extraManifests is populated, validate that the specified configmap has been applied and is valid
    - If a required CRD is missing from the current stateroot, a warning message will be included in the prep status condition for user to verify before proceeding to the upgrade stage
  - Validate that the desired upgrade version matches the version of the seed image
  - Validate the version of the LCA in the seed image is compatible with the version on the running SNO
- Unpack the seed image and create a new ostree stateroot
- Pull all images specified by the image list built into the seed image. Refer to [precache-plugin](precache-plugin.md)

Upon completion, the condition will be updated to "Prep Completed"

Condition samples:

Prep in progress:

```console
  conditions:
  - lastTransitionTime: "2024-04-19T19:25:29Z"
    message: In progress
    observedGeneration: 5
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-04-19T19:25:29Z"
    message: Setting up stateroot
    observedGeneration: 5
    reason: InProgress
    status: "True"
    type: PrepInProgress
  observedGeneration: 5
  validNextStages:
  - Idle

  conditions:
  - lastTransitionTime: "2024-04-19T19:25:29Z"
    message: In progress
    observedGeneration: 5
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-04-19T19:25:29Z"
    message: 'Precaching progress: total: 115 (pulled: 10, failed: 0)'
    observedGeneration: 5
    reason: InProgress
    status: "True"
    type: PrepInProgress
  observedGeneration: 5
  validNextStages:
  - Idle
```

Prep completed:

```console
  conditions:
  - lastTransitionTime: "2024-04-19T19:25:29Z"
    message: In progress
    observedGeneration: 5
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-04-19T19:26:52Z"
    message: Prep completed
    observedGeneration: 5
    reason: Completed
    status: "False"
    type: PrepInProgress
  - lastTransitionTime: "2024-04-19T19:26:52Z"
    message: Prep completed successfully
    observedGeneration: 5
    reason: Completed
    status: "True"
    type: PrepCompleted
  observedGeneration: 5
  validNextStages:
  - Idle
  - Upgrade
```

#### Starting the Upgrade stage

This is where the actual upgrade happens. It consists of three main steps: pre-pivot, pivot and post-pivot.
This stage can only be applied if the prep stage completed successfully.

The administrator patches the imagebasedupgrade CR to set the stage to "Upgrade"

```console
oc patch imagebasedupgrades.lca.openshift.io upgrade -p='{"spec": {"stage": "Upgrade"}}' --type=merge
```

Pre-pivot:

- LCA collects the required cluster specific info/artifacts and stores them in the new state root. This includes hostname, nmconnection files, cluster ID, NodeIP and various OCP platform CRs(i.e., imagecontentimagecontentsourcepolicies, localvolumes.local.storage.openshift.io) from etcd.
- If LVMS is used, automatically update dynamic PVs with `.spec.persistentVolumeReclaimPolicy=Delete` to `Retain`.
- Applies OADP backup CRs as specified by the `oadpContent` field in the IBU spec. Refer to [backuprestore-with-oadp](backuprestore-with-oadp.md).
- Stores OADP restore CRs as specified by the `oadpContent` field in the IBU spec to the new state root. Refer to [backuprestore-with-oadp](backuprestore-with-oadp.md).
- Stores CRs specified by the `extraManifests` field in the IBU spec as well as the CRs described in the ZTP policies bound to the cluster for the target OCP version to the new state root.
- Stores LVM config to the new state root.
- Stores a copy of the IBU CR to the new state root.
- Set the new default deployment.

Pivot

- Initiate a controlled reboot.

Post-Pivot

- Before OCP is started, a systemd service will run which will restore the basic platform configuration and regenerate the platform certificates using the [recert tool](https://github.com/rh-ecosystem-edge/recert).
- Once LCA starts, it will restore the saved IBU CR.
- Update the IBU CR status to set the `rollbackAvailabilityExpiration` timestamp, reflecting the latest point at which a rollback could be performed without potentially
  requiring manual actions to recover from expired control plane certificates on the original stateroot.
- Restore the remaining platform configuration.
- Wait for the platform to recover - Cluster/day2 operators and MCP are stable.
- Apply extra manifests that were saved pre-pivot.
- Apply any OADP restore CRs that were saved pre-pivot. Platform artifacts will be restored first, including ACM artifacts if the system is managed by ACM.
- If LVMS is used, automatically restores the `.spec.persistentVolumeReclaimPolicy` field of PVs to its original (pre-pivot) value.

Upon completion, the condition will be updated to `Upgrade completed`.

After the upgrade has been completed, the upgrade needs to be finalized. This can be done at anytime prior to the next upgrade attempt.
This is the point of no return, it will be no longer be possible to rollback/abort once the finalize has been done.
Refer to [Finalizing or Aborting](#finalizing-or-aborting)

Condition samples:

Upgrade in progress, pre-pivot:

```console
status:
  conditions:
  - lastTransitionTime: "2024-04-19T19:25:29Z"
    message: In progress
    observedGeneration: 5
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-04-19T19:26:52Z"
    message: Prep completed
    observedGeneration: 5
    reason: Completed
    status: "False"
    type: PrepInProgress
  - lastTransitionTime: "2024-04-19T19:26:52Z"
    message: Prep completed successfully
    observedGeneration: 5
    reason: Completed
    status: "True"
    type: PrepCompleted
  - lastTransitionTime: "2024-04-19T19:28:14Z"
    message: |-
      Waiting for system to stabilize: one or more health checks failed
        - one or more ClusterOperators not yet ready: authentication
        - one or more MachineConfigPools not yet ready: master
        - one or more ClusterServiceVersions not yet ready: sriov-fec.v2.8.0
    observedGeneration: 1
    reason: InProgress
    status: "True"
    type: UpgradeInProgress
  observedGeneration: 1
  rollbackAvailabilityExpiration: "2024-05-19T14:01:52Z"
  validNextStages:
  - Rollback
```

Upgrade in progress, post-pivot:

```console
```

Upgrade completed:

```console
status:
  conditions:
  - lastTransitionTime: "2024-04-19T19:25:29Z"
    message: In progress
    observedGeneration: 5
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-04-19T19:26:52Z"
    message: Prep completed
    observedGeneration: 5
    reason: Completed
    status: "False"
    type: PrepInProgress
  - lastTransitionTime: "2024-04-19T19:26:52Z"
    message: Prep completed successfully
    observedGeneration: 5
    reason: Completed
    status: "True"
    type: PrepCompleted
  - lastTransitionTime: "2024-04-19T19:43:55Z"
    message: Upgrade completed
    observedGeneration: 1
    reason: Completed
    status: "False"
    type: UpgradeInProgress
  - lastTransitionTime: "2024-04-19T19:43:55Z"
    message: Upgrade completed
    observedGeneration: 1
    reason: Completed
    status: "True"
    type: UpgradeCompleted
  observedGeneration: 1
  rollbackAvailabilityExpiration: "2024-05-19T14:01:52Z"
  validNextStages:
  - Idle
  - Rollback
```

### Rollback after Pivot

Upgrade(post-pivot) -> Rollback

It can be set if upgrade has gone beyond the pivot step, whether it has completed, failed or still in progress.
LCA performs the rollback by setting the original state root as default and rebooting the node.

```console
oc patch imagebasedupgrades.lca.openshift.io upgrade -p='{"spec": {"stage": "Rollback"}}' --type=merge
```

After the rollback has been completed, the system will be running the original state root.
It will be necessary to finalize the rollback to attempt another upgrade.
Refer to [Finalizing or Aborting](#finalizing-or-aborting)

### Automatic Rollback on Upgrade Failure

In an IBU, the LCA provides capability for automatic rollback upon failure at certain points of the upgrade, after the
Upgrade stage reboot. The automatic rollback feature is enabled by default in an IBU.

In the case of an upgrade failure that automatically triggers a rollback, the IBU CR status will be updated to indicate
the reason for the rollback. This may include information about the specific error that occurred to help
troubleshooting.

See [Automatic Rollback Examples](examples.md#automatic-rollback-examples) for examples of IBU CR after an automatic rollback.

#### Configuring Automatic Rollback

There is an `lca-init-monitor.service` that runs post-reboot with a configurable timeout. When LCA marks
the upgrade complete, it shuts down this monitor. If this point is not reached within the configured timeout, the
init-monitor will trigger an automatic rollback.:

- Configure the timeout value, in seconds, by setting `.spec.autoRollbackOnFailure.initMonitorTimeoutSeconds`

These values can be set via patch command, for example:

```console
# Set the init-monitor timeout to one hour. The default timeout is 30 minutes (1800 seconds)
oc patch imagebasedupgrades.lca.openshift.io upgrade --type=merge -p='{"spec": {"autoRollbackOnFailure": {"initMonitorTimeoutSeconds": 3600}}}'

# Set the init-monitor timeout to five minutes. The default timeout is 30 minutes (1800 seconds)
oc patch imagebasedupgrades.lca.openshift.io upgrade --type=merge -p='{"spec": {"autoRollbackOnFailure": {"initMonitorTimeoutSeconds": 300}}}'

# Reset to default
oc patch imagebasedupgrades.lca.openshift.io upgrade --type json -p='[{"op": "replace", "path": "/spec/autoRollbackOnFailure", "value": {} }]'
```

Alternatively, use `oc edit ibu upgrade` and add the following to the `.spec` section:

```yaml
spec:
  autoRollbackOnFailure:
    initMonitorTimeoutSeconds: 3600
```

### Finalizing or Aborting

After a successful upgrade or rollback the stage must be set to "Idle" to cleanup and prepare for the next upgrade.

It is also possible to abort the upgrade at any point prior to the pivot by setting the stage to "Idle" .

Transitions:

- Prep or Upgrade(pre-pivot) -> Idle
- Rollback -> Idle
- Upgrade(post-pivot) -> Idle

```console
oc patch imagebasedupgrades.lca.openshift.io upgrade -p='{"spec": {"stage": "Idle"}}' --type=merge
```

This will:

- Remove the old state root
- Cleanup precaching resources
- Delete OADP backups CRs
- Remove IBU files from the file system

Once completed, the system is ready for the next upgrade.

#### Finalize or Abort failure

If finalize or abort fails, IBU transitions into `FinalizeFailed` or
`AbortFailed` states respectively.

```yaml
  status:
    conditions:
    - message: failed to delete all the backup CRs. Perform cleanup manually then add 'lca.openshift.io/manual-cleanup-done' annotation to ibu CR to transition back to Idle
      observedGeneration: 5
      reason: AbortFailed
      status: "False"
      type: Idle
```

The condition message indicates which parts of the cleanup have failed.
User should perform the [cleanup manually](troubleshooting.md#manual-cleanup) in these states.
After manual cleanup user should add `lca.openshift.io/manual-cleanup-done` annotation
to IBU CR.

```yaml
  kind: ImageBasedUpgrade
  metadata:
    annotations:
      lca.openshit.io/manual-cleanup-done: ""

```

Upon observing this annotation, IBU will remove the annotation and run
abort/finalize again. If successful, IBU transitions back to the `Idle` state.

### Monitoring Progress

LCA Operator logs:

```console
oc logs -n openshift-lifecycle-agent --selector app.kubernetes.io/component=lifecycle-agent --container manager --follow
```
