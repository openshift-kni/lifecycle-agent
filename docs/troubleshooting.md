# Troubleshooting Guide

## Reading USB-based IBI prepare results off-box (ibi-status / p4)

USB-based image-based-install (IBI) media carries a small writable `ext4` partition labelled
`ibi-status` (the "p4" partition). The prepare phase records a machine-readable per-phase
result there, so you can learn how a prepare ended without a serial console — just move the
USB stick (or the node's p4 partition) to a workstation.

1. Identify the partition by its filesystem label and mount it read-only:

   ```console
   lsblk -o NAME,LABEL,FSTYPE | grep ibi-status
   sudo mount -o ro /dev/disk/by-label/ibi-status /mnt/ibi-status
   ```

2. Read the prepare result at the partition **root**:

   ```console
   cat /mnt/ibi-status/prepare.json
   ```

   - `result: "success"` — the prepare phase completed. On install-only media a completed
     prepare powers the node off; on self-contained media it pivots (reboots into the installed
     disk) instead.
   - `result: "failure"` — the prepare phase failed; `error` carries the message. On a
     failure the node **halts powered-on** (emergency target) rather than powering off, so it
     stays reachable for inspection. A later boot will **not** automatically retry over a
     recorded failure (the disk may already be partly wiped); the prepare phase blocks it as a
     re-entry (below). To retry, perform an **explicit operator reset** — clear `prepare.json`
     from the `ibi-status` partition — after confirming the installation disk is in a safe
     state to re-wipe.
   - `reentry: "blocked"` — a later boot re-entered a partition that already records a terminal
     result — a **completed** install (`result: "success"`) or a **prior failed** prepare
     (`result: "failure"`) — and the prepare phase refused to re-wipe the disk. The original
     result is preserved. This is expected and protective. If the original result was
     `success`, pull the stick (or stop selecting USB as the boot device) so the installed node
     boots from disk. If it was `failure`, resolve the underlying failure and perform the
     explicit operator reset above before retrying.
   - `pivot: "failed"` — a self-contained prepare succeeded (`result: "success"` is preserved)
     but the on-USB pivot into the installed disk did not take: the reboot command errored, or
     the reboot was issued but the node had not gone down after the grace period (the original
     prepare process was still running). This is recorded only by that original process, so it
     means the reboot itself did not proceed — not that the firmware re-selected the USB. A
     second boot that lands back in the live ISO (because the firmware booted the USB again) is
     instead caught on that next boot by the re-entry guard and recorded as `reentry: "blocked"`
     (above), so check the boot order there. The node **halts powered-on** (emergency target)
     rather than idling in the live ISO. `error` stays `null` (the prepare itself succeeded);
     the reboot error or timeout is in the log bundle under `logs/` (refreshed on the
     pivot-failure path so it covers the pivot attempt) — read it to find why the reboot did
     not take, then reboot the node into the installed disk manually.

3. Read the compact diagnostic log bundle under `logs/` for the full prepare journal:

   ```console
   zcat /mnt/ibi-status/logs/prepare-logs.txt.gz | less
   ```

> If `prepare.json` is **absent**, the prepare phase never reached the result write-back —
> check whether p4 could be mounted writable at all (a missing, duplicated, or read-only
> `ibi-status` partition makes prepare halt powered-on *before* any disk wipe, by design).

See [create-usb.md](./create-usb.md#the-ibi-status-p4-partition-and-per-phase-results) for
the full p4 contract and schema.

## Manual cleanup

During Abort/Finalize LCA performs a cleanup. This includes:

- Remove the old state root
- Cleanup precaching resources
- Delete OADP backups CRs
- Remove IBU files from the file system

If LCA fails to perform any of the above steps, it transitions to Abort/Finalize failed state.
The condition message and log shows which steps have failed.
User should perform manual cleanup and add `lca.openshift.io/manual-cleanup-done` annotation to IBU. After observing this annotation LCA will retry the cleanup and if successful, IBU transitions to `Idle`.

### Stateroot cleanup

During `Abort` LCA cleanups the new stateroot and during `Finalize` LCA cleanups the old stateroot. User should inspect the log to determine why this step has failed since failure in this step indicates a serious issue in the node.
You can add the `lca.openshift.io/manual-cleanup-done` annotation to IBU so that LCA retries the cleanup. If manual cleanup of a stateroot is necessary follow the following steps:

- Run `ostree admin status` to determine if there are any existing deployments in the stateroot. These will need to be cleaned up first. Cleanup every deployment in the stateroot using:

```bash
ostree admin undeploy <0 based index of deployment> 
```

- After cleaning up all the deployments of the stateroot, you can wipe the stateroot folder:

> [!CAUTION]
> Make sure the booted deployment is not in this stateroot.

```bash
stateroot="<stateroot to be deleted>"
unshare -m /bin/sh -c "mount -o remount,rw /sysroot && rm -rf /sysroot/ostree/deploy/${stateroot}"
```

### OADP backup cleanup

The main reason that this step can fail is connection failure between LCA and the backup backend. By restoring the connection and adding `lca.openshift.io/manual-cleanup-done` annotation, LCA can successfully cleanup backup resources.

The following command can be used to check the backend connectivity:

```bash
$ oc get backupstoragelocations.velero.io -n openshift-adp
NAME                          PHASE       LAST VALIDATED   AGE   DEFAULT
dataprotectionapplication-1   Available   33s              8d    true
```

Otherwise, user should manually remove all backup resources `backups.velero.io`, `restores.velero.io` and `deletebackuprequests.velero.io`. Then add `lca.openshift.io/manual-cleanup-done` annotation to IBU CR.

## LVMS volume contents not restored

When LVMS is used to provide dynamic PV storage, it may not restore the PV contents when it is not configured correctly.
Below, we provide some common symptoms and solutions in order to aid the user while troubleshooting such cases.

### Missing lvms-related fields in Backup CR

#### Symptom (missing backups fields)

```shell
-> oc describe pod <sample-app-name>

... TRUNCATED ...

Events:
  Type     Reason            Age                From               Message
  ----     ------            ----               ----               -------
  Warning  FailedScheduling  58s (x2 over 66s)  default-scheduler  0/1 nodes are available: pod has unbound immediate PersistentVolumeClaims. preemption: 0/1 nodes are available: 1 Preemption is not helpful for scheduling..
  Normal   Scheduled         56s                default-scheduler  Successfully assigned default/db-777b7ff898-g8gg6 to sno1.inbound.bos2.lab
  Warning  FailedMount       24s (x7 over 55s)  kubelet            MountVolume.SetUp failed for volume "pvc-e8bf06ad-62a2-418b-ab29-c09e64d5550e" : rpc error: code = Unknown desc = VolumeID is not found
```

#### Resolution (missing backups fields)

The reason for this issue is that the application has restored its PVC and PV manifests correctly, but the
`logicalvolume` associated to this PV hasn't been restored properly after pivot.
Steps for its resolution were described in [Application backup and restore CRs](backuprestore-with-oadp.md#application-backup-and-restore-crs),
and basically relates to also include `logicalvolumes.topolvm.io` in the application's Backup CR (see sample below).

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
  includedClusterScopedResources:
  - persistentVolumes              # <- required field
  - volumesnapshotcontents         # <- required field
  - logicalvolumes.topolvm.io      # <- required field
```

### Missing lvms-related fields in Restore CR

#### Symptom (missing restore fields)

The same expected resources for the app are restored (see symptoms below), but the **PV contents were not preserved
after upgrading**.

before pivot

```shell
-> oc get pv,pvc,logicalvolumes.topolvm.io -A

NAME                                                        CAPACITY   ACCESS MODES   RECLAIM POLICY   STATUS   CLAIM            STORAGECLASS   REASON   AGE
persistentvolume/pvc-e8bf06ad-62a2-418b-ab29-c09e64d5550e   1Gi        RWO            Retain           Bound    default/pvc-db   lvms-vg1                4h45m

NAMESPACE   NAME                           STATUS   VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS   AGE
default     persistentvolumeclaim/pvc-db   Bound    pvc-e8bf06ad-62a2-418b-ab29-c09e64d5550e   1Gi        RWO            lvms-vg1       4h45m

NAMESPACE   NAME                                                                AGE
            logicalvolume.topolvm.io/pvc-e8bf06ad-62a2-418b-ab29-c09e64d5550e   4h45m
```

after pivot

```shell
-> oc get pv,pvc,logicalvolumes.topolvm.io -A

NAME                                                        CAPACITY   ACCESS MODES   RECLAIM POLICY   STATUS   CLAIM            STORAGECLASS   REASON   AGE
persistentvolume/pvc-e8bf06ad-62a2-418b-ab29-c09e64d5550e   1Gi        RWO            Delete           Bound    default/pvc-db   lvms-vg1                19s

NAMESPACE   NAME                           STATUS   VOLUME                                     CAPACITY   ACCESS MODES   STORAGECLASS   AGE
default     persistentvolumeclaim/pvc-db   Bound    pvc-e8bf06ad-62a2-418b-ab29-c09e64d5550e   1Gi        RWO            lvms-vg1       19s

NAMESPACE   NAME                                                                AGE
            logicalvolume.topolvm.io/pvc-e8bf06ad-62a2-418b-ab29-c09e64d5550e   18s
```

> **Note:** The PVs contents can be easily recovered at any moment by just setting rollback in the IBU CR.

#### Resolution (missing restore fields)

The reason for this issue was that the `logicalvolume` status was not preserved in the Restore CR, this is important
because it explicitly instructs Velero about the reference to the volumes that need to be preserved after pivoting.

Steps were described in [Application backup and restore CRs](backuprestore-with-oadp.md#application-backup-and-restore-crs),
and basically relates to also include the corresponding fields in the application's Restore CR (see sample below).

```yaml
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: sample-vote-app
  namespace: openshift-adp
  labels:
    velero.io/storage-location: default
  annotations:
    lca.openshift.io/apply-wave: "3"
spec:
  backupName:
    sample-vote-app
  restorePVs: true                 # <- required field
  restoreStatus:                   # <- required field
    includedResources:             # <- required field
      - logicalvolumes             # <- required field
```
