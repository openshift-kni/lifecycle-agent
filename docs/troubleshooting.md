# Troubleshooting Guide

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
