# Developing in lifecycle-agent-operator

## Prerequisites

- go >=1.18
- Spoke SNO cluster with pull-secret containing credentials to quay.io/${MY_REPO_ID}/

## Makefile targets

To see all `make` targets, run `make help`

## Linter tests

As of this writing, three linters are run by the `make ci-job` command:

* golangci-lint
* shellcheck
* bashate

These tests will be run automatically as part of the ci-job test post a pull request an update. Failures will mean your pull request
cannot be merged. It is recommended that you run `make ci-job` regularly as part of development.

## GO formatting

GO has automated formatting. To update code and ensure it is formatted properly, run:<br>`make fmt`

## Updates to bindata

When updating files in a `bindata` directory (eg. `internal/bindata/`), you will need to regenerate the
corresponding go code by running:<br>
`make update-bindata`

## Building and deploying image

There are make variables you can set when building the image to customize how it is built and tagged. For example, you can set
ENGINE=podman if your build system uses podman instead of docker. To use a custom repository, you can use the IMAGE_TAG_BASE variable.
make IMAGE_TAG_BASE=quay.io/dpenney/lifecycle-agent-operator VERSION=latest ENGINE=podman update-bindata

For example:

```console
# Update bindata, then build and push the image
make IMAGE_TAG_BASE=quay.io/${MY_REPO_ID}/lifecycle-agent-operator VERSION=latest ENGINE=podman \
    update-bindata \
    docker-build \
    docker-push

# Deploy the operator to your SNO (with KUBECONFIG set appropriately)
make IMAGE_TAG_BASE=quay.io/${MY_REPO_ID}/lifecycle-agent-operator VERSION=latest ENGINE=podman \
    install \
    deploy

# Watch LCA logs
oc logs -n openshift-lifecycle-agent --selector app.kubernetes.io/name=lifecyle-agent-operator --timestamps --follow
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
        "image": "quay.io/dpenney/upgbackup:lca-test-seed-v1",
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

## Running the ibu-imager

With the IBU Imager packaged into the LCA image, it can be run on the seed SNO with the LCA deployed.

```console
# Get the LCA Image reference
export KUBECONFIG=/etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/lb-ext.kubeconfig
LCA_IMAGE=$(oc get deployment -n openshift-lifecycle-agent lifecycle-agent-controller-manager -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')

# Login to your repo and generate an authfile
AUTHFILE=/tmp/backup-secret.json
podman login --authfile ${AUTHFILE} -u ${MY_ID} quay.io/${MY_REPO_ID}

export IMG_REFSPEC=quay.io/${MY_REPO_ID}/${MY_REPO}:${MY_TAG}
export IMG_RECERT_TOOL=quay.io/edge-infrastructure/recert:latest

podman run --privileged --pid=host --rm --net=host \
    -v /etc:/etc \
    -v /var:/var \
    -v /var/run:/var/run \
    -v /run/systemd/journal/socket:/run/systemd/journal/socket \
    -v ${AUTHFILE}:${AUTHFILE} \
    --entrypoint ibu-imager ${LCA_IMAGE} create --authfile ${AUTHFILE} \
                                                 --image ${IMG_REFSPEC} \
                                                 --recert-image ${IMG_RECERT_TOOL}
```

#### Disconnected environments

For disconnected environments, first mirror the `lifecycle-agent` container image to your local registry using
[skopeo](https://github.com/containers/skopeo) or a similar tool.

Additionally, you'd need to mirror the [recert](https://github.com/rh-ecosystem-edge/recert) tool, which is used to
perform some checks in the seed SNO cluster.

```shell
skopeo copy docker://quay.io/edge-infrastructure/recert docker://${LOCAL_REGISTRY}/edge-infrastructure/recert
````

## Setup dev backup steps

### minio + oadp oprator

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
  channel: stable-1.2
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
you can re-configure the pre-caching to `best-effort` by running the sequence of `shell` and `oc` commands provided below.

```shell
# Get current list of env vars for the lifecycle-agent-controller-manager deployment
ENV_VARS=$(oc get deployment -n openshift-lifecycle-agent lifecycle-agent-controller-manager -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].env}' | jq -c)

# Append the pre-caching best-effort environment variable  
UPDATED_ENV_VARS=$(echo "$ENV_VARS" | jq --argjson new_var '{"name":"PRECACHE_BEST_EFFORT","value":"TRUE"}' '. + [$new_var]' | jq -c)

# Patch the lifecycle-agent-controller-manager deployment
oc patch deployment lifecycle-agent-controller-manager -n openshift-lifecycle-agent --type=json -p='[{"op": "add", "path": "/spec/template/spec/containers/0/env", "value": '"$UPDATED_ENV_VARS"'}]'

# Confirm the patching was successful - you should see `{"name":"PRECACHE_BEST_EFFORT","value":"TRUE"}` in the output
oc get deployment -n openshift-lifecycle-agent lifecycle-agent-controller-manager -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].env}'
```

Note that the patching will result in the termination of the current lifecycle-agent-controller-manager pod and the 
creation of a new one. The leader election may take a few minutes to complete. 

Once the pod is up and running (with leader election completed), you may proceed to create the IBU CR as part of the 
normal workflow.

## Generating Seed Image via LCA Orchestration

Generation of the seed image can be triggered by creating a `SeedGenerator` CR named `seedimage`. The CR requires the
following data to be set:

- `seedImage`: The pullspec (ie. registry/repo:tag) for the generated image
- `recertImage`: (Optional) Allows user to specify the recert tool image to pass to ibu-imager

In addition, a `Secret` named `seedgen` is required in the `openshift-lifecycle-agent` namespace. This allows the user
to provide the following information in the `Data` section:

- `seedAuth`: base64-encoded auth file for write-access to the registry for pushing the generated seed image
- `hubKubeconfig`: (Optional) base64-encoded kubeconfig for admin access to the hub, in order to deregister the seed
  cluster from ACM. If this is not present in the secret, the ACM cleanup will be skipped.

If the `hubKubeconfig` is provided, LCA will deregister the cluster from ACM and cleanup the node prior to generating
the seed image. The `ManagedCluster` resource is saved prior to deletion from the hub in order to restore it after the
seed image has been generated, reregistering the cluster with ACM.

NOTE: Automatic restore of the `ManagedCluster` has not yet been implemented.

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
  hubKubeconfig: <encoded kubeconfig>
---
apiVersion: lca.openshift.io/v1alpha1
kind: SeedGenerator
metadata:
  name: seedimage
spec:
  seedImage: quay.io/dpenney/upgbackup:orchestrated-seed-image
  recertImage: quay.io/edge-infrastructure/recert:latest
```
