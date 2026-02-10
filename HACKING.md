# Developing in lifecycle-agent-operator

## Prerequisites

- go >=1.18
- Spoke SNO cluster with pull-secret containing credentials to quay.io/${MY_REPO_ID}/

## Makefile targets

To see all `make` targets, run `make help`

## Linter tests

As of this writing, three linters are run by the `make ci-job` command:

- golangci-lint
- shellcheck
- bashate

These tests will be run automatically as part of the ci-job test post a pull request an update. Failures will mean your pull request
cannot be merged. It is recommended that you run `make ci-job` regularly as part of development.

Additionally, markdownlint can be run manually using `make markdownlint` or `make ENGINE=podman markdownlint`. This
`Makefile` target will build a local container image with the markdownlint tool, which is then run to validate the
markdown files in the repo.

## GO formatting

GO has automated formatting. To update code and ensure it is formatted properly, run: `make fmt`

## Building and deploying image

There are make variables you can set when building the image to customize how it is built and tagged. For example, you can set
`ENGINE=podman` if your build system uses podman instead of docker. To use a custom repository, you can use the `IMAGE_TAG_BASE` variable.

For example:

```console
# Build and push the image
make IMAGE_TAG_BASE=quay.io/${MY_REPO_ID}/lifecycle-agent-operator VERSION=latest ENGINE=podman \
    docker-push

# Deploy the operator to your SNO (with KUBECONFIG set appropriately)
make IMAGE_TAG_BASE=quay.io/${MY_REPO_ID}/lifecycle-agent-operator VERSION=latest ENGINE=podman \
    install \
    deploy
```

Alternatively, you can also build your own bundle image and deploy it through OLM as an operator:

```console
make IMAGE_TAG_BASE=quay.io/${MY_REPO_ID}/lifecycle-agent-operator VERSION=latest ENGINE=podman \
    bundle-push \
    bundle-run
```

To watch LCA logs:

```console
oc logs -n openshift-lifecycle-agent --selector app.kubernetes.io/name=lifecycle-agent-operator -c manager --follow
```

## Creating CR and Updating Stage

```console
# Update the IBU CR, specifying the seed image
oc patch imagebasedupgrade upgrade -n openshift-lifecycle-agent --type='json' -p='[
  {
    "op": "replace",
    "path": "/spec",
    "value": {
      "oadpContent": [{"name": "oadp-cm", "namespace": "openshift-adp"}],
      "stage": "Idle",
      "seedImageRef": {
        "version": "4.13.10",
        "image": "quay.io/myrepo/upgbackup:lca-test-seed-v1",
        "pullSecretRef": {"name": "seed-pull-secret"}
      }
    }
  }
]'

# Set the stage to Prep
oc patch imagebasedupgrades.lca.openshift.io upgrade -p='{"spec": {"stage": "Prep"}}' --type=merge

# Set the stage back to Idle
oc patch imagebasedupgrades.lca.openshift.io upgrade -p='{"spec": {"stage": "Idle"}}' --type=merge
```

## Cleanup

```console
# Delete LCA resources
make undeploy

# Delete LCA installed as an operator
make bundle-clean

#
# Delete the deployment and stateroot (on the SNO)
#
function get_deployment_id {
    local stateroot="$1"
    for ((i=0;i<$(rpm-ostree status --json | jq -r '.deployments | length');i++)); do
        if [ "$(rpm-ostree status --json | jq -r --argjson id $i '.deployments[$id].osname')" = "${stateroot}" ]; then
            echo $i
            return
        fi
    done
}

stateroot=rhcos_4.13.9
deployment_id=$(get_deployment_id ${stateroot})
if [ -n "${deployment_id}" ]; then
    ostree admin undeploy ${deployment_id} && unshare -m /bin/sh -c "mount -o remount,rw /sysroot && rm -rf /sysroot/ostree/deploy/${stateroot}"
else
    echo "No deployment found for ${stateroot}"
fi

# Delete the IBU files
rm -f /var/ibu/*
```

## Running the lca-cli

With the lca-cli packaged into the LCA image, it can be run on the seed SNO with the LCA deployed.

```console
# Get the LCA Image reference
export KUBECONFIG=/etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/lb-ext.kubeconfig
LCA_IMAGE=$(oc get deployment -n openshift-lifecycle-agent lifecycle-agent-controller-manager -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')

# Login to your repo and generate an authfile
AUTHFILE=/tmp/backup-secret.json
podman login --authfile ${AUTHFILE} -u ${MY_ID} quay.io/${MY_REPO_ID}

export IMG_REFSPEC=quay.io/${MY_REPO_ID}/${MY_REPO}:${MY_TAG}
export IMG_RECERT_TOOL=quay.io/edge-infrastructure/recert:v0

podman run --privileged --pid=host --rm --net=host \
    -v /etc:/etc \
    -v /var:/var \
    -v /var/run:/var/run \
    -v /run/systemd/journal/socket:/run/systemd/journal/socket \
    -v ${AUTHFILE}:${AUTHFILE} \
    --entrypoint lca-cli ${LCA_IMAGE} create --authfile ${AUTHFILE} \
                                                 --image ${IMG_REFSPEC} \
                                                 --recert-image ${IMG_RECERT_TOOL}
```

## Disconnected environments

For disconnected environments, first mirror the `lifecycle-agent` container image to your local registry using
[skopeo](https://github.com/containers/skopeo) or a similar tool.

Additionally, you'd need to mirror the [recert](https://github.com/rh-ecosystem-edge/recert) tool, which is used to
perform some checks in the seed SNO cluster.

```shell
skopeo copy docker://quay.io/edge-infrastructure/recert docker://${LOCAL_REGISTRY}/edge-infrastructure/recert
````

## Setup dev backup steps

### minio + oadp operator

Consider using podman if you have a HV

```shell
podman run --name minio -d -p 9000:9000 -p 9001:9001 quay.io/minio/minio server /data --console-address ":9001"
```

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: minio-dev
  labels:
    name: minio-dev

---
apiVersion: v1
kind: Pod
metadata:
  labels:
    app: minio
  name: minio
  namespace: minio-dev
spec:
  containers:
  - name: minio
    image: quay.io/minio/minio:latest
    command:
    - /bin/bash
    - -c
    args:
    - minio server /data --console-address :9090
    volumeMounts:
    - mountPath: /var/local/data
      name: localvolume
  volumes:
  - name: localvolume
    hostPath:
      path: /var/local/data
      type: DirectoryOrCreate

---
apiVersion: v1
kind: Service
metadata:
  name: minio
  namespace: minio-dev
spec:
  selector:
    app: minio
  ports:
    - name: two
      protocol: TCP
      port: 9090
    - name: one
      protocol: TCP
      port: 9000
# kubectl port-forward svc/minio 9000 9090 -n minio-dev
---
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-adp
  labels:
    name: openshift-adp

---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: openshift-adp
  namespace: openshift-adp
spec:
  targetNamespaces:
  - openshift-adp

---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: openshift-adp
  namespace: openshift-adp
spec:
  channel: stable-1.3
  installPlanApproval: Automatic
  approved: true
  name: redhat-oadp-operator
  source: redhat-operators-disconnected
  sourceNamespace: openshift-marketplace
```

### setup DataProtectionApplication CR

```yaml
# before applying this setup creds for the bucket access + create bucket
# create bucket: aws s3api create-bucket --bucket test-backups --profile minio
# create a new called `credentials-velero`
#
# [default]
# aws_access_key_id=testkey
# aws_secret_access_key=testaccesskey
#
# apply: oc create secret generic cloud-credentials -n openshift-adp --from-file cloud=credentials-velero

apiVersion: oadp.openshift.io/v1alpha1
kind: DataProtectionApplication
metadata:
  name: dataprotectionapplication
  namespace: openshift-adp
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
          profile: "default" # todo find out why it didnt work with a different profile name
          region: minio
          s3Url: "http://minio.minio-dev.svc.cluster.local:9000"
          insecureSkipTLSVerify: "true"
          s3ForcePathStyle: "true"
        provider: aws
        default: true
        credential:
          key: cloud
          name: cloud-credentials
        objectStorage:
          bucket: test-backups
          prefix: velero # a required value if no image provided
```

## How to view Pre-caching logs (from the Prep stage)

The precaching functionality is implemented as a Kubernetes job. Therefore, The precaching logs are separate from the
lifecycle-agent operator logs. The precaching logs can be viewed as shown below.

```shell
oc logs -f --tail=-1 --timestamps --prefix --selector job-name=lca-precache-job -n openshift-lifecycle-agent
```

Note that the logs are available until the user triggers a clean-up through an abort process.

## Setting Pre-caching to 'best-effort'

By default, the pre-caching job is configured to fail if it encounters any error, or it fails to pull one or more images.

Should you wish to change the pre-caching mode to exit successfully even though there was a failure to pull an image,
you can re-configure the pre-caching to `best-effort` by setting `PRECACHE_BEST_EFFORT=TRUE` in the LCA deployment
environment variables..

```shell
# Set the env variable for the manager container in the deployment
oc set env -n openshift-lifecycle-agent deployments.apps/lifecycle-agent-controller-manager -c manager PRECACHE_BEST_EFFORT=TRUE

# To check the list of env variables in the deployment
oc set env -n openshift-lifecycle-agent deployments.apps/lifecycle-agent-controller-manager --list

# To check the env in the running container
oc rsh -n openshift-lifecycle-agent -c manager \
    $(oc get pods -n openshift-lifecycle-agent -l app.kubernetes.io/component=lifecycle-agent --no-headers -o custom-columns=NAME:.metadata.name) \
    env

# To drop the env variable
oc set env -n openshift-lifecycle-agent deployments.apps/lifecycle-agent-controller-manager -c manager PRECACHE_BEST_EFFORT-
```

Note that the patching will result in the termination of the current lifecycle-agent-controller-manager pod and the
creation of a new one. The leader election may take a few minutes to complete.

Once the pod is up and running (with leader election completed), you may proceed to create the IBU CR as part of the
normal workflow.

## Generating Seed Image via LCA Orchestration

Generation of the seed image can be triggered by creating a `SeedGenerator` CR named `seedimage`. The CR requires the
following data to be set:

- `seedImage`: The pullspec (i.e., registry/repo:tag) for the generated image
- `recertImage`: (Optional) Allows user to specify the recert tool image to pass to the image builder

> :warning: **If the recert image is given as a registry moving tag (e.g. "latest" or "v0") the image could change between seed generation and post-pivot recert.**
Recert makes no guarantees that a seed invalidated using one version can be "fixed" by a newer version.
To ensure your seed image is using the same recert image it's recommended to mirror the recert image and use SHA reference in the seedgen CR

In addition, a `Secret` named `seedgen` is required in the `openshift-lifecycle-agent` namespace. This allows the user
to provide the following information in the `Data` section:

- `seedAuth`: base64-encoded auth file for write-access to the registry for pushing the generated seed image

```yaml
---
apiVersion: v1
kind: Secret
metadata:
  name: seedgen
  namespace: openshift-lifecycle-agent
type: Opaque
data:
  seedAuth: <encoded authfile>
---
apiVersion: lca.openshift.io/v1
kind: SeedGenerator
metadata:
  name: seedimage
spec:
  seedImage: quay.io/myrepo/upgbackup:orchestrated-seed-image
  recertImage: quay.io/edge-infrastructure/recert:v0
```

## Rollbacks

After the cluster has rebooted to the new release as part of the Upgrade stage handling, a controlled Rollback can be
triggered by setting the IBU stage to "Rollback". LCA will automatically pivot to the original stateroot, if supported
(manual pivot is required otherwise), and reboot the cluster. Once the cluster recovers on the original stateroot, LCA
will update the CR to indicate that the rollback is complete. At this point, the rollback can be finalized by setting
the stage to Idle, triggering cleanup of the upgrade stateroot.

In the case of an uncontrolled rollback, such as a manual reboot to the original stateroot, LCA will update the CR to
indicate that the upgrade has failed. The stage can then be set to Idle to trigger the abort handler.

### Manually setting new default deployment

The `ostree admin set-default` command is used to set a new default (ie. next boot) deployment. It is available as of
4.14.7. If available, LCA will automatically set the new default deployment as part of triggering the rollback.

If the `set-default` command is not available, LCA is unable to set the new default deployment automatically, requiring
it to be set manually. As a workaround for earlier releases without this command, a dev image has been built with the
newer ostree cli that can be used.

Note that deployment 0 is the default deployment, when viewing `ostree admin status` output. The argument passed to the
`set-default` command is the deployment index. So to select the second deployment in the list as the new default, run
`ostree admin set-default 1`. Also note that `/sysroot` and `/boot` must be remounted with write-access if using the dev
image workaround for earlier releases.

```console
# Using "ostree admin set-default"
[root@cnfdf01 core]# ostree admin status
* rhcos_4.14.7 abb6d9631dc78117e7f6058bfab5f94ceff6328f2916fb887715b156f8620cbc.0
    origin: <unknown origin type>
  rhcos 16189fd979d620d0df256f41b8fdf345ad05f2dbaf56f99ed21ff81c695e676c.0
    origin: <unknown origin type>
[root@cnfdf01 core]# ostree admin set-default 1
Bootloader updated; bootconfig swap: yes; bootversion: boot.0.1, deployment count change: 0
[root@cnfdf01 core]# ostree admin status
  rhcos 16189fd979d620d0df256f41b8fdf345ad05f2dbaf56f99ed21ff81c695e676c.0
    origin: <unknown origin type>
* rhcos_4.14.7 abb6d9631dc78117e7f6058bfab5f94ceff6328f2916fb887715b156f8620cbc.0
    origin: <unknown origin type>

# Using quay.io/openshift-kni/telco-ran-tools:test-ostree-cli
[root@cnfdf01 core]# ostree admin status
* rhcos_4.14.7 abb6d9631dc78117e7f6058bfab5f94ceff6328f2916fb887715b156f8620cbc.0
    origin: <unknown origin type>
  rhcos 16189fd979d620d0df256f41b8fdf345ad05f2dbaf56f99ed21ff81c695e676c.0
    origin: <unknown origin type>
[root@cnfdf01 core]# mount /sysroot -o remount,rw ; mount /boot -o remount,rw
[root@cnfdf01 core]# podman run --rm --privileged -v /sysroot:/sysroot -v /boot:/boot -v /ostree:/ostree quay.io/openshift-kni/telco-ran-tools:test-ostree-cli ostree admin set-default 1
Bootloader updated; bootconfig swap: yes; bootversion: boot.0.1, deployment count change: 0
[root@cnfdf01 core]# ostree admin status
  rhcos 16189fd979d620d0df256f41b8fdf345ad05f2dbaf56f99ed21ff81c695e676c.0
    origin: <unknown origin type>
* rhcos_4.14.7 abb6d9631dc78117e7f6058bfab5f94ceff6328f2916fb887715b156f8620cbc.0
    origin: <unknown origin type>
```

## Automatic Container Storage Cleanup on Prep

At the start of the Prep stage, LCA checks the container storage disk usage. If the usage exceeds a certain threshold, which defaults to 50%, it will delete images
from container storage until the disk usage is within the threshold. The list of image removal candidates is sorted by the image's Created timestamp, oldest to newest,
with dangling images deleted first. Images that are in use or that are pinned are skipped.

This automated container storage cleanup can be disabled by setting an `image-cleanup.lca.openshift.io/on-prep='Disabled'` annotation on the IBU CR. Additionally, the default
disk usage threshold value can be overridden by setting an `image-cleanup.lca.openshift.io/disk-usage-threshold-percent` annotation.

```console
# Disabling automatic image cleanup
oc -n  openshift-lifecycle-agent annotate ibu upgrade image-cleanup.lca.openshift.io/on-prep='Disabled'

# Removing annotation to re-enable automatic image cleanup
oc -n  openshift-lifecycle-agent annotate ibu upgrade image-cleanup.lca.openshift.io/on-prep-

# Overriding default disk usage threshold to 65%
oc -n  openshift-lifecycle-agent annotate ibu upgrade image-cleanup.lca.openshift.io/disk-usage-threshold-percent='65'

# Removing threshold override
oc -n  openshift-lifecycle-agent annotate ibu upgrade image-cleanup.lca.openshift.io/disk-usage-threshold-percent-
```

### Pinning Images in CRI-O

Images can be pinned in CRI-O by setting the `crio.image/pinned_images` list in crio.conf. This can be done by adding a custom file in `/etc/crio/crio.conf.d`.
See [crio.conf.md](https://github.com/cri-o/cri-o/blob/main/docs/crio.conf.5.md#crioimage-table) for more information.

Example:

```console
# cat /etc/crio/crio.conf.d/50-pinned-images
[crio.image]
# List of images to be excluded from the kubelet's garbage collection.
# It allows specifying image names using either exact, glob, or keyword
# patterns. Exact matches must match the entire name, glob matches can
# have a wildcard * at the end, and keyword matches can have wildcards
# on both ends. By default, this list includes the "pause" image if
# configured by the user, which is used as a placeholder in Kubernetes pods.
pinned_images = [
    "localhost/testimage:1",
    "localhost/testimage:3",
    "localhost/testimage:5",
    "localhost/testimage:7",
    "localhost/testimage:9",
    "localhost/testimage:11",
    "localhost/testimage:13",
    "localhost/testimage:15",
    "localhost/testimage:17",
    "localhost/testimage:19",
]
```

## Automatic Rollback on Failure

In an IBU, the LCA provides capability for automatic rollback upon failure at
certain points of the upgrade after the Upgrade stage reboot. In addition,
there is an `lca-init-monitor.service` that runs post-reboot with a
configurable timeout. When LCA marks the upgrade complete, it shuts down this
monitor. If this point is not reached within the configured timeout, the
init-monitor will trigger an automatic rollback.

The automatic rollback feature is enabled by default in an IBU. To disable it, you can annotate the IBU CR:

- Disable rollback when the reconfiguration of the cluster fails upon the first reboot using the `auto-rollback-on-failure.lca.openshift.io/post-reboot-config: Disabled` annotation.
- Disable rollback after the Lifecycle Agent reports a failed upgrade upon completion using the `auto-rollback-on-failure.lca.openshift.io/upgrade-completion: Disabled` annotation.
- Disable rollback from LCA Init Monitor watchdog using the `auto-rollback-on-failure.lca.openshift.io/init-monitor: Disabled` annotation.

These values can be set via patch command, for example:

```console
oc -n  openshift-lifecycle-agent annotate ibu upgrade auto-rollback-on-failure.lca.openshift.io/post-reboot-config='Disabled'
oc -n  openshift-lifecycle-agent annotate ibu upgrade auto-rollback-on-failure.lca.openshift.io/upgrade-completion='Disabled'
oc -n  openshift-lifecycle-agent annotate ibu upgrade auto-rollback-on-failure.lca.openshift.io/init-monitor='Disabled'

# Reset to default
oc -n  openshift-lifecycle-agent annotate ibu upgrade auto-rollback-on-failure.lca.openshift.io/post-reboot-config-
oc -n  openshift-lifecycle-agent annotate ibu upgrade auto-rollback-on-failure.lca.openshift.io/upgrade-completion-
oc -n  openshift-lifecycle-agent annotate ibu upgrade auto-rollback-on-failure.lca.openshift.io/init-monitor-
```

Alternatively, use `oc edit ibu upgrade` and add the following to the `.spec` section:

```yaml
metadata:
  annotations:
    auto-rollback-on-failure.lca.openshift.io/post-reboot-config: Disabled
    auto-rollback-on-failure.lca.openshift.io/upgrade-completion: Disabled
    auto-rollback-on-failure.lca.openshift.io/init-monitor: Disabled
```

You can also configure the timeout value by setting `.spec.autoRollbackOnFailure.initMonitorTimeoutSeconds`

This value can be set via patch command, for example:

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
metadata:
  annotations:
    auto-rollback-on-failure.lca.openshift.io/init-monitor: Disabled
spec:
  autoRollbackOnFailure:
    initMonitorTimeoutSeconds: 3600
```
