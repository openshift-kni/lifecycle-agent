# Developing in lifecycle-agent-operator

## Makefile targets

To see all `make` targets, run `make help`

## Linter tests

As of this writing, four linters are run by the `make ci-job` command:

* golint
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
    docker-build
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
# Generate the IBU CR, specifying the seed image
oc create -f - <<EOF
apiVersion: ran.openshift.io/v1alpha1
kind: ImageBasedUpgrade
metadata:
  name: upgrade
spec:
  stage: Idle
  seedImageRef:
    version: 4.13.9
    image: quay.io/dpenney/upgbackup:lca-test-seed-v1
EOF

# Set the stage to Prep
oc patch imagebasedupgrades.ran.openshift.io upgrade -p='{"spec": {"stage": "Prep"}}' --type=merge

# Set the stage back to Idle
oc patch imagebasedupgrades.ran.openshift.io upgrade -p='{"spec": {"stage": "Idle"}}' --type=merge
```

## Cleanup
```console
# Delete LCA resources
oc delete imagebasedupgrades.ran.openshift.io upgrade ; \
oc delete ns openshift-lifecycle-agent ; \
oc delete customresourcedefinition.apiextensions.k8s.io/imagebasedupgrades.ran.openshift.io \
    clusterrole.rbac.authorization.k8s.io/lifecycle-agent-manager-role \
    clusterrole.rbac.authorization.k8s.io/lifecycle-agent-metrics-reader \
    clusterrole.rbac.authorization.k8s.io/lifecycle-agent-proxy-role \
    clusterrolebinding.rbac.authorization.k8s.io/lifecycle-agent-manager-rolebinding \
    clusterrolebinding.rbac.authorization.k8s.io/lifecycle-agent-proxy-rolebinding 

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

