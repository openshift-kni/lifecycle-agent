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

### The reconfigure result (`reconfigure.json`)

On **self-contained** media the same `ibi-status` (p4) partition also records the
**reconfigure convergence verdict** at `reconfigure.json`, written after the node pivots into
the installed disk and reconfigures itself. Two independent writers may produce it, but it is
single-owner per outcome:

- The **pre-recert clock gate** writes a `failure` (with a `clock` section) if the system clock
  is implausible before recert — the node halts powered-on and recert never runs.
- The **reconfigure result-writer** (a dedicated `lca-reconfigure-result-writer.service` unit)
  owns the convergence verdict after recert: `success` once the node is `Ready` and all cluster
  operators have converged, or `failure` if that criterion is not met within the convergence
  timeout (default **60 minutes**, configurable via `siteConfig.reconfigureConvergenceTimeout`).

Read it the same way as `prepare.json`:

```console
cat /mnt/ibi-status/reconfigure.json
```

- `result: "success"` — the node converged; it keeps running and the result-writer unit
  disables itself. Nothing to do.
- `result: "failure"` — convergence was not reached in time (or the clock gate failed before
  recert); `error` carries the message. The node **halts powered-on** (emergency target) so it
  stays reachable.

> **Keep the USB inserted during reconfigure.** The result-writer writes `reconfigure.json`
> to the USB's p4 partition after the pivot, so the stick must remain inserted until a terminal
> verdict is recorded. Only stop booting from the USB (change the boot order) rather than
> physically removing it before the verdict lands.

Both verdicts are **idempotent**: an existing terminal `success` or `failure` is never
overwritten across reboots, so a later boot cannot flip a recorded verdict.

**Recovery after a `failure`.** First identify *which* failure you hit — the two have different
recoveries, distinguished by the `error` message and by whether the durable handoff
(`/var/lib/lca/reconfigure-handoff.json`) exists. A clock-gate failure happens **before** the
handoff is created; a convergence timeout happens **after** it exists.

**Clock-gate failure (before recert — no handoff yet).** The pre-recert clock gate runs *before*
post-pivot writes the handoff, so on an implausible clock it records the `failure` and halts the
node before recert — the result-writer never runs and no convergence anchor was ever created
(`/var/lib/lca/reconfigure-handoff.json` is absent). Do **not** try to renew an anchor; there is
none. Correct the clock and re-run post-pivot so the gate re-evaluates and, on success, writes the
handoff that gates the result-writer. Run these **on the node**:

1. Fix the clock / trusted time source so the gate will pass.
2. Clear the recorded verdict:

   ```console
   sudo mount /dev/disk/by-label/ibi-status /mnt/ibi-status
   sudo rm -f /mnt/ibi-status/reconfigure.json
   sudo umount /mnt/ibi-status
   ```

3. Return the node to normal operation and re-run post-pivot. A clock-gate `failure` halts the
   node by isolating `emergency.target`, so cluster services are stopped.
   `installation-configuration.service` is still enabled (post-pivot halted before disabling it),
   so the cleanest recovery is a **reboot** — the node comes up in `multi-user.target`, the gate
   re-evaluates the corrected clock, and on success post-pivot writes the handoff **and the
   completion marker** that let the result-writer start automatically:

   ```console
   sudo systemctl reboot
   ```

   If a reboot is undesirable, return to `multi-user.target` in place. Because
   `installation-configuration.service` is still enabled, isolating the target re-runs post-pivot
   **once**; on success post-pivot writes the handoff and completion marker and then starts the
   result-writer itself, so no further commands are needed:

   ```console
   sudo systemctl isolate multi-user.target
   ```

   Do **not** run `sudo systemctl restart installation-configuration.service` afterward. That first
   post-pivot run completes and its cleanup removes the configuration directory, so a second run
   would find no configuration and wait indefinitely, blocking recovery.

**Convergence timeout (after recert — handoff exists).** Clearing the failure is not enough. The
convergence deadline is anchored **write-once** at `postPivotStartTime` in the durable handoff, so
simply re-running the writer keeps the original, now-expired deadline by design — it would
immediately record `failure` again without ever polling convergence, even if the node has since
converged. The result-writer unit also disables itself after writing a terminal verdict. A
supported recovery therefore resolves the cause, grants the writer a **new** anchor, clears the
verdict, and re-enables the unit. Run these **on the node** (a `failure` halts it powered-on, so it
stays reachable):

1. Resolve what blocked convergence.
2. Renew the convergence anchor by setting `postPivotStartTime` to the current time in the
   durable handoff. Do **not** delete the handoff — a missing handoff makes the writer a no-op,
   so no verdict is ever recorded:

   ```console
   sudo sed -i "s|\"postPivotStartTime\": \".*\"|\"postPivotStartTime\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"|" \
     /var/lib/lca/reconfigure-handoff.json
   ```

3. Clear the recorded verdict so the writers no longer treat it as terminal, then unmount so the
   result-writer can remount the partition itself:

   ```console
   sudo mount /dev/disk/by-label/ibi-status /mnt/ibi-status
   sudo rm -f /mnt/ibi-status/reconfigure.json
   sudo umount /mnt/ibi-status
   ```

4. Restore normal operation **before** starting the writer. A `failure` halts the node by
   isolating `emergency.target`, which stops the cluster services (kubelet, CRI-O, the control
   plane) the node needs in order to converge. Starting the writer while the node is still in
   `emergency.target` leaves those services stopped, so it would only time out again. Return the
   node to `multi-user.target` first, then run the writer. Either:

   - **Reboot** (recommended — restores `multi-user.target` and all cluster services cleanly; the
     condition-gated writer then runs automatically once it is enabled):

     ```console
     sudo systemctl enable lca-reconfigure-result-writer.service
     sudo systemctl reboot
     ```

   - **or** switch targets in place without rebooting, then start the writer:

     ```console
     sudo systemctl isolate multi-user.target
     sudo systemctl enable --now lca-reconfigure-result-writer.service
     ```

> Only this explicit operator recovery renews the anchor. Automatic
> `installation-configuration.service` `Restart=on-failure` retries deliberately keep the original
> write-once deadline unchanged, so they never silently extend the convergence window.

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
