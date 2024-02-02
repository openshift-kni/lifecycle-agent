# Image Based Upgrade

- [Image Based Upgrade](#image-based-upgrade)
  - [Overview](#overview)
  - [Handling Site Specific Artifacts](#handling-site-specific-artifacts)
    - [Extra Manifests](#extra-manifests)
    - [Backup and Restore](#backup-and-restore)
  - [Target SNO Prerequisites](#target-sno-prerequisites)
  - [ImageBasedUpgrade CR](#imagebasedupgrade-cr)
  - [Image Based Upgrade Walkthrough](#image-based-upgrade-walkthrough)
    - [Success Path](#success-path)
      - [Starting the Prep stage](#starting-the-prep-stage)
      - [Starting the Upgrade stage](#starting-the-upgrade-stage)
    - [Rollback after Pivot](#rollback-after-pivot)
    - [Automatic Rollback on Upgrade Failure](#automatic-rollback-on-upgrade-failure)
      - [Configuring Automatic Rollback](#configuring-automatic-rollback)
    - [Finalizing or Aborting](#finalizing-or-aborting)
    - [Monitoring Progress](#monitoring-progress)

## Overview

Image Based Upgrade (IBU) is a mechanism that allows a SNO to be upgraded via a seed image. The seed is an OCI image
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

Two mechanisms are provided for the admin to apply site specific artifacts to the SNO following the pivot to the new version.

### Extra Manifests

In order to optimize the downtime of IBU, the intent is to apply as much configuration as possible to the seed image.
Some manifests contain site specific data that cannot be part of the seed image. Furthermore, these manifests may be OCP
version specific so cannot be simply backed up and restored. The Life Cycle Agent provides a mechanism to apply a set of
extra manifests after pivoting to the new OCP version. These manifests are stored in configmap(s) and specified by the
`extraManifests` field [IBU CR](#imagebasedupgrade-cr)

### Backup and Restore

This is another mechanism provided to apply site specific artifacts to the new OCP version. This is mainly intended for
the applications running on the SNO whose artifacts will not change as a result of the OCP version change. It will also
be used for a small number of platform artifacts that will not change over the upgrade (ex. ACM klusterlet). The OADP
operator is used to implement the backup and restore functionality. A configmap(s) is applied to the cluster and
specified by the `oadpContent` field in [IBU CR](#imagebasedupgrade-cr). This configmap will contain a set of OADP
backup and restore CR. Refer to [backuprestore-with-oadp](backuprestore-with-oadp.md).

## Target SNO Prerequisites

The target SNO has the following prerequisites:

- A compatible seed image has been generated [Seed Image Generation](seed-image-generation.md) and pushed to a registry accessible from the SNO
- The CPU topology must align with the seed SNO.
  - Same number of cores.
  - Same performance tuning (i.e., reserved CPUs).
- LCA operator must be deployed, version must be compatible with the seed.
- The OADP operator is installed along with a DataProtectionApplication CR. OADP has connectivity to a S3 backend

## ImageBasedUpgrade CR

The spec fields include:

- stage: defines the desired stage for the IBU (Idle, Prep, Upgrade or Rollback)
- seedImageRef: defines the target OCP version, the seed image to be used and the secret required for accessing the image
- oadpContent: defines the list of config maps where the OADP backup / restore CRs are stored. This is optional
- extraManifests: defines the list of config maps where the additional CRs to be re-applied are stored
- autoRollbackOnFailure: configures the auto-rollback feature for upgrade failure, which is enabled by default
  - disabledForPostRebootConfig: set to `true` to disable auto-reboot for the LCA post-reboot config service-units
    - Service unit `prepare-installation-configuration.service` performs network configuration updates
    - Service unit `installation-configuration.service` transforms cluster data from the seed image to the target
      cluster
  - disabledForUpgradeCompletion: set to `true` to disable auto-reboot for the LCA Upgrade completion handler, which
    performs tasks such as data restore and application of extra-manifests
  - disabledInitMonitor: set to `true` to disable the LCA Init Monitor, which is a post-reboot watchdog that triggers a
    rollback if the upgrade is not completed within the configured timeout
  - initMonitorTimeoutSeconds: set the LCA Init Monitor timeout duration, in seconds. The default value is 1800 (30 minutes).
    Setting a value less than or equal to 0 will use the default

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
  generation: 1
  name: upgrade
spec:
  autoRollbackOnFailure: {}
  stage: Idle
status:
  conditions:
  - lastTransitionTime: "2024-01-19T05:42:58Z"
    message: Idle
    observedGeneration: 1
    reason: Idle
    status: "True"
    type: Idle
  observedGeneration: 1
```

## Image Based Upgrade Walkthrough

The Lifecycle Agent provides orchestration of the image based upgrade, triggered by patching the `ImageBasedUpgrade` CR through a series of stages.

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
  - Validate that the desired upgrade version matches the version of the seed image
  - Validate the version of the LCA in the seed image is compatible with the version on the running SNO
- Unpack the seed image and create a new ostree stateroot
- Pull all images specified by the image list built into the seed image. Refer to [precache-plugin](precache-plugin.md)

Upon completion, the condition will be updated to "Prep Completed"

Condition samples:

Prep in progress:

```console
  conditions:
  - lastTransitionTime: "2024-01-19T06:26:06Z"
    message: In progress
    observedGeneration: 2
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-01-19T06:26:06Z"
    message: Pulling seed image
    observedGeneration: 2
    reason: InProgress
    status: "True"
    type: PrepInProgress
  observedGeneration: 2

  conditions:
  - lastTransitionTime: "2024-01-19T06:26:06Z"
    message: In progress
    observedGeneration: 2
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-01-19T06:26:06Z"
    message: 'Precaching progress: total: 228 (pulled: 21, skipped: 20, failed: 0)'
    observedGeneration: 2
    reason: InProgress
    status: "True"
    type: PrepInProgress
  observedGeneration: 2
```

Prep completed:

```console
  conditions:
  - lastTransitionTime: "2024-01-19T06:26:06Z"
    message: In progress
    observedGeneration: 2
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-01-19T06:30:36Z"
    message: Prep completed
    observedGeneration: 2
    reason: Completed
    status: "False"
    type: PrepInProgress
  - lastTransitionTime: "2024-01-19T06:30:36Z"
    message: 'Prep completed successfully: total: 228 (pulled: 208, skipped: 20, failed: 0)'
    observedGeneration: 2
    reason: Completed
    status: "True"
    type: PrepCompleted
  observedGeneration: 2
```

#### Starting the Upgrade stage

This is where the actual upgrade happens. It consists of three main steps: pre-pivot, pivot and post-pivot.
This stage can only be applied if the prep stage completed successfully.

The administrator patches the imagebasedupgrade CR to set the stage to "Upgrade"

```console
oc patch imagebasedupgrades.lca.openshift.io upgrade -p='{"spec": {"stage": "Upgrade"}}' --type=merge
```

Pre-pivot:

- LCA collects the required cluster specific info/artifacts and stores them in the new state root. This includes hostname, nmconnection files, cluster ID, NodeIP and various OCP platform CRs from etcd.
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
- Once LCA starts it will restore the saved IBU CR.
- Restore the remaining platform configuration.
- Wait for the platform to recover - Cluster/day2 operators and MCP are stable.
- Apply extra manifests that were saved pre-pivot.
- Apply any OADP restore CRs that were saved pre-pivot. Platform artifacts will be restored first including ACM artifacts if the system is managed by ACM.

Upon completion, the condition will be updated to "Upgrade Completed".

After the upgrade has been completed, the upgrade needs to be finalized. This can be done at anytime prior to the next upgrade attempt.
This is the point of no return, it will be no longer be possible to rollback/abort once the finalize has been done.
Refer to [Finalizing or Aborting](#finalizing-or-aborting)

Condition samples:

Upgrade in progress:

```console
  conditions:
  - lastTransitionTime: "2024-01-19T06:26:06Z"
    message: In progress
    observedGeneration: 2
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-01-19T06:30:36Z"
    message: Prep completed
    observedGeneration: 2
    reason: Completed
    status: "False"
    type: PrepInProgress
  - lastTransitionTime: "2024-01-19T06:30:36Z"
    message: 'Prep completed successfully: total: 228 (pulled: 208, skipped: 20, failed: 0)'
    observedGeneration: 2
    reason: Completed
    status: "True"
    type: PrepCompleted
  - lastTransitionTime: "2024-01-19T06:40:08Z"
    message: In progress
    observedGeneration: 3
    reason: InProgress
    status: "True"
    type: UpgradeInProgress
  observedGeneration: 3
```

Upgrade completed:

```console
  conditions:
  - lastTransitionTime: "2024-01-19T06:26:06Z"
    message: In progress
    observedGeneration: 2
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-01-19T06:30:36Z"
    message: Prep completed
    observedGeneration: 2
    reason: Completed
    status: "False"
    type: PrepInProgress
  - lastTransitionTime: "2024-01-19T06:30:36Z"
    message: 'Prep completed successfully: total: 228 (pulled: 208, skipped: 20, failed: 0)'
    observedGeneration: 2
    reason: Completed
    status: "True"
    type: PrepCompleted
  - lastTransitionTime: "2024-01-19T06:54:29Z"
    message: Upgrade completed
    observedGeneration: 1
    reason: Completed
    status: "False"
    type: UpgradeInProgress
  - lastTransitionTime: "2024-01-19T06:54:29Z"
    message: Upgrade completed
    observedGeneration: 1
    reason: Completed
    status: "True"
    type: UpgradeCompleted
  observedGeneration: 1
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

To disable automatic rollback, there are configuration options in the `ImageBasedUpgrade` CRD that can be defined:

- In the `prepare-installation-configuration.service` systemd service-unit (`prepare-installation-configuration`). Disabled by
  setting `.spec.autoRollbackOnFailure.disabledForPostRebootConfig: true` in the IBU `upgrade` CR
- In the `installation-configuration.service` systemd service-unit (`lca-cli postpivot`). Disabled by
  setting `.spec.autoRollbackOnFailure.disabledForPostRebootConfig: true` in the IBU `upgrade` CR
- In the LCA IBU post-reboot Upgrade stage handler. Disabled by
  setting `.spec.autoRollbackOnFailure.disabledForUpgradeCompletion: true` in the IBU `upgrade` CR

These values can be set via patch command, for example:

```console
# Disable automatic rollback for the post-reboot config service-units
oc patch imagebasedupgrades.lca.openshift.io upgrade --type=merge -p='{"spec": {"autoRollbackOnFailure": {"disabledForPostRebootConfig": true}}}'

# Disable automatic rollback for the post-reboot Upgrade stage handler
oc patch imagebasedupgrades.lca.openshift.io upgrade --type=merge -p='{"spec": {"autoRollbackOnFailure": {"disabledForUpgradeCompletion": true}}}'

# Reset to default
oc patch imagebasedupgrades.lca.openshift.io upgrade --type json -p='[{"op": "replace", "path": "/spec/autoRollbackOnFailure", "value": {} }]'
```

Alternatively, use `oc edit ibu upgrade` and add the following to the `.spec` section:

```yaml
  autoRollbackOnFailure:
    disabledForPostRebootConfig: true
    disabledForUpgradeCompletion: true
```

In addition, there is an `lca-init-monitor.service` that runs post-reboot with a configurable timeout. When LCA marks
the upgrade complete, it shuts down this monitor. If this point is not reached within the configured timeout, the
init-monitor will trigger an automatic rollback. This can be configured via the `.spec.autoRollbackOnFailure` fields:

- To disable the init-monitor automatic rollback, set `.spec.autoRollbackOnFailure.disabledInitMonitor` to `true`
- Configure the timeout value, in seconds, by setting `.spec.autoRollbackOnFailure.initMonitorTimeoutSeconds`

These values can be set via patch command, for example:

```console
# Disable automatic rollback for the init-monitor
oc patch imagebasedupgrades.lca.openshift.io upgrade --type=merge -p='{"spec": {"autoRollbackOnFailure": {"disabledInitMonitor": true}}}'

# Set the init-monitor timeout to one hour. The default timeout is 30 minutes (1800 seconds)
oc patch imagebasedupgrades.lca.openshift.io upgrade --type=merge -p='{"spec": {"autoRollbackOnFailure": {"initMonitorTimeoutSeconds": 3600}}}'

# Set the init-monitor timeout to five minutes. The default timeout is 30 minutes (1800 seconds)
oc patch imagebasedupgrades.lca.openshift.io upgrade --type=merge -p='{"spec": {"autoRollbackOnFailure": {"initMonitorTimeoutSeconds": 300}}}'

# Reset to default
oc patch imagebasedupgrades.lca.openshift.io upgrade --type json -p='[{"op": "replace", "path": "/spec/autoRollbackOnFailure", "value": {} }]'
```

Alternatively, use `oc edit ibu upgrade` and add the following to the `.spec` section:

```yaml
  autoRollbackOnFailure:
    disabledInitMonitor: true
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

### Monitoring Progress

LCA Operator logs:

```console
oc logs -n openshift-lifecycle-agent --selector app.kubernetes.io/component=lifecycle-agent --container manager --follow
```
