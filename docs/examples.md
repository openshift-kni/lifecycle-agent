# Image Based Upgrade Examples

- [Image Based Upgrade Examples](#image-based-upgrade-examples)
  - [Automatic Rollback Examples](#automatic-rollback-examples)
    - [Failure in `prepare-installation-configuration.service`](#failure-in-prepare-installation-configurationservice)
      - [Example IBU Brief Display for `prepare-installation-configuration.service` Failure Rollback](#example-ibu-brief-display-for-prepare-installation-configurationservice-failure-rollback)
      - [Example IBU Detailed Display for `prepare-installation-configuration.service` Failure Rollback](#example-ibu-detailed-display-for-prepare-installation-configurationservice-failure-rollback)
    - [Failure in `installation-configuration.service`](#failure-in-installation-configurationservice)
      - [Example IBU Brief Display for `installation-configuration.service` Failure Rollback](#example-ibu-brief-display-for-installation-configurationservice-failure-rollback)
      - [Example IBU Detailed Display for `installation-configuration.service` Failure Rollback](#example-ibu-detailed-display-for-installation-configurationservice-failure-rollback)
    - [LCA Init Monitor Watchdog Timeout](#lca-init-monitor-watchdog-timeout)
      - [Example IBU Brief Display for Watchdog Timeout Rollback](#example-ibu-brief-display-for-watchdog-timeout-rollback)
      - [Example IBU Detailed Display for Watchdog Timeout Rollback](#example-ibu-detailed-display-for-watchdog-timeout-rollback)

## Automatic Rollback Examples

### Failure in `prepare-installation-configuration.service`

#### Example IBU Brief Display for `prepare-installation-configuration.service` Failure Rollback

```console
$ oc get ibu
NAME      AGE    STAGE     STATE    DETAILS
upgrade   110s   Upgrade   Failed   Rollback due to service-unit failure: component config
```

#### Example IBU Detailed Display for `prepare-installation-configuration.service` Failure Rollback

```console
$ oc get ibu upgrade -o yaml
apiVersion: lca.openshift.io/v1alpha1
kind: ImageBasedUpgrade
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"lca.openshift.io/v1alpha1","kind":"ImageBasedUpgrade","metadata":{"annotations":{},"name":"upgrade"},"spec":{"extraManifests":[{"name":"sno-extramanifests","namespace":"openshift-lifecycle-agent"}],"seedImageRef":{"image":"quay.io/user/seedimage:4.15.0","version":"4.15.0"},"stage":"Idle"}}
  creationTimestamp: "2024-02-01T19:20:40Z"
  generation: 1
  name: upgrade
  resourceVersion: "2950109"
  uid: 6d60bc28-bf69-4222-b9ff-e6da197495ae
spec:
  additionalImages:
    name: ""
    namespace: ""
  autoRollbackOnFailure: {}
  extraManifests:
  - name: sno-extramanifests
    namespace: openshift-lifecycle-agent
  seedImageRef:
    image: quay.io/user/seedimage:4.15.0
    version: 4.15.0
  stage: Upgrade
status:
  conditions:
  - lastTransitionTime: "2024-02-01T19:00:45Z"
    message: In progress
    observedGeneration: 2
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-02-01T19:03:15Z"
    message: Prep completed
    observedGeneration: 2
    reason: Completed
    status: "False"
    type: PrepInProgress
  - lastTransitionTime: "2024-02-01T19:03:15Z"
    message: Prep completed successfully
    observedGeneration: 2
    reason: Completed
    status: "True"
    type: PrepCompleted
  - lastTransitionTime: "2024-02-01T19:09:19Z"
    message: Upgrade failed
    observedGeneration: 1
    reason: Failed
    status: "False"
    type: UpgradeCompleted
  - lastTransitionTime: "2024-02-01T19:09:19Z"
    message: 'Rollback due to service-unit failure: component config'
    observedGeneration: 1
    reason: Failed
    status: "False"
    type: UpgradeInProgress
  observedGeneration: 1
```

### Failure in `installation-configuration.service`

#### Example IBU Brief Display for `installation-configuration.service` Failure Rollback

```console
$ oc get ibu
NAME      AGE   STAGE     STATE    DETAILS
upgrade   75s   Upgrade   Failed   Rollback due to postpivot failure: failed to run recert tool container: Trying to pull quay.io/user/recert:invalidtag......
```

#### Example IBU Detailed Display for `installation-configuration.service` Failure Rollback

```console
$ oc get ibu -o yaml
apiVersion: v1
items:
- apiVersion: lca.openshift.io/v1alpha1
  kind: ImageBasedUpgrade
  metadata:
    annotations:
      kubectl.kubernetes.io/last-applied-configuration: |
        {"apiVersion":"lca.openshift.io/v1alpha1","kind":"ImageBasedUpgrade","metadata":{"annotations":{},"name":"upgrade"},"spec":{"extraManifests":[{"name":"sno-extramanifests","namespace":"openshift-lifecycle-agent"}],"seedImageRef":{"image":"quay.io/user/seedimage:4.15.0","version":"4.15.0"},"stage":"Idle"}}
    creationTimestamp: "2024-02-01T20:00:48Z"
    generation: 1
    name: upgrade
    resourceVersion: "2959121"
    uid: c7e46afa-3ac1-4cdc-8be1-53f3e71d6385
  spec:
    additionalImages:
      name: ""
      namespace: ""
    autoRollbackOnFailure: {}
    extraManifests:
    - name: sno-extramanifests
      namespace: openshift-lifecycle-agent
    seedImageRef:
      image: quay.io/user/seedimage:4.15.0
      version: 4.15.0
    stage: Upgrade
  status:
    conditions:
    - lastTransitionTime: "2024-02-01T19:45:56Z"
      message: In progress
      observedGeneration: 3
      reason: InProgress
      status: "False"
      type: Idle
    - lastTransitionTime: "2024-02-01T19:47:56Z"
      message: Prep completed
      observedGeneration: 3
      reason: Completed
      status: "False"
      type: PrepInProgress
    - lastTransitionTime: "2024-02-01T19:47:56Z"
      message: Prep completed successfully
      observedGeneration: 3
      reason: Completed
      status: "True"
      type: PrepCompleted
    - lastTransitionTime: "2024-02-01T19:49:34Z"
      message: Upgrade failed
      observedGeneration: 1
      reason: Failed
      status: "False"
      type: UpgradeCompleted
    - lastTransitionTime: "2024-02-01T19:49:34Z"
      message: |-
        Rollback due to postpivot failure: failed to run recert tool container: Trying to pull quay.io/user/recert:invalidtag...
        time="2024-02-01T19:55:28Z" level=warning msg="Failed, retrying in 1s ... (1/3). Error: initializing source docker://quay.io/user/recert:invalidtag: reading manifest recert in quay.io/user/recert: unknown: Tag invalidtag was deleted or has expired. To pull, revive via time machine"
        time="2024-02-01T19:55:29Z" level=warning msg="Failed, retrying in 1s ... (2/3). Error: initializing source docker://quay.io/user/recert:invalidtag: reading manifest recert in quay.io/user/recert: unknown: Tag invalidtag was deleted or has expired. To pull, revive via time machine"
        time="2024-02-01T19:55:30Z" level=warning msg="Failed, retrying in 1s ... (3/3). Error: initializing source docker://quay.io/user/recert:invalidtag: reading manifest recert in quay.io/user/recert: unknown: Tag invalidtag was deleted or has expired. To pull, revive via time machine"
        Error: initializing source docker://quay.io/user/recert:invalidtag: reading manifest recert in quay.io/user/recert: unknown: Tag invalidtag was deleted or has expired. To pull, revive via time machine
        : exit status 125
      observedGeneration: 1
      reason: Failed
      status: "False"
      type: UpgradeInProgress
    observedGeneration: 1
kind: List
metadata:
  resourceVersion: ""
```

### LCA Init Monitor Watchdog Timeout

Examples shown with LCA Init Monitor watchdog timeout configured to 5 minutes, rather than the default 30 minutes.

#### Example IBU Brief Display for Watchdog Timeout Rollback

```console
$ oc get ibu
NAME      AGE     STAGE     STATE    DETAILS
upgrade   3m33s   Upgrade   Failed   Rollback due to LCA Init Monitor timeout, after 5m0s
```

#### Example IBU Detailed Display for Watchdog Timeout Rollback

```console
$ oc get ibu upgrade -o yaml
apiVersion: lca.openshift.io/v1alpha1
kind: ImageBasedUpgrade
metadata:
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"lca.openshift.io/v1alpha1","kind":"ImageBasedUpgrade","metadata":{"annotations":{},"name":"upgrade"},"spec":{"extraManifests":[{"name":"sno-extramanifests","namespace":"openshift-lifecycle-agent"}],"seedImageRef":{"image":"quay.io/user/seedimage:4.15.0","version":"4.15.0"},"stage":"Idle"}}
  creationTimestamp: "2024-02-01T21:22:36Z"
  generation: 1
  name: upgrade
  resourceVersion: "2969303"
  uid: 95b02356-cfe3-4dc1-ba96-b0d3b7d21a82
spec:
  additionalImages:
    name: ""
    namespace: ""
  autoRollbackOnFailure:
    initMonitorTimeoutSeconds: 300
  extraManifests:
  - name: sno-extramanifests
    namespace: openshift-lifecycle-agent
  seedImageRef:
    image: quay.io/user/seedimage:4.15.0
    version: 4.15.0
  stage: Upgrade
status:
  conditions:
  - lastTransitionTime: "2024-02-01T21:00:36Z"
    message: In progress
    observedGeneration: 4
    reason: InProgress
    status: "False"
    type: Idle
  - lastTransitionTime: "2024-02-01T21:02:36Z"
    message: Prep completed
    observedGeneration: 4
    reason: Completed
    status: "False"
    type: PrepInProgress
  - lastTransitionTime: "2024-02-01T21:02:36Z"
    message: Prep completed successfully
    observedGeneration: 4
    reason: Completed
    status: "True"
    type: PrepCompleted
  - lastTransitionTime: "2024-02-01T21:03:25Z"
    message: Upgrade failed
    observedGeneration: 1
    reason: Failed
    status: "False"
    type: UpgradeCompleted
  - lastTransitionTime: "2024-02-01T21:03:25Z"
    message: Rollback due to LCA Init Monitor timeout, after 5m0s
    observedGeneration: 1
    reason: Failed
    status: "False"
    type: UpgradeInProgress
  observedGeneration: 1
```
