# Manual steps for the creation of an OCI seed image

This guide is heavily inspired by the great work done in [this script](https://github.com/eranco74/sno-relocation-poc/blob/master/ostree-backup.sh).

```shell
# env
export REGISTRY_AUTH_FILE=/var/lib/kubelet/config.json
export LOCAL_REGISTRY=jumphost.inbound.vz.bos2.lab:8443
export KUBECONFIG=/etc/kubernetes/static-pod-resources/kube-apiserver-certs/secrets/node-kubeconfigs/lb-ext.kubeconfig

export base_tag=base
export parent_tag=parent
export backup_tag=backup
export backup_repo=${1:-$LOCAL_REGISTRY/ostmagic}
export backup_refspec=$backup_repo:$backup_tag
export base_refspec=$backup_repo:$base_tag
export parent_refspec=$backup_repo:$parent_tag
```


# 1) Saving list of running containers and clusterversion

```shell
mkdir -pv /var/tmp/backup
crictl ps -o json| jq -r '.containers[] | .imageRef' > /var/tmp/backup/containers.list
oc get clusterversion version -ojson > /var/tmp/backup/clusterversion.json
```


# 2) Stopping kubelet / containers / crio

```shell
systemctl stop kubelet
crictl ps -q | xargs --no-run-if-empty crictl stop --timeout 5
while crictl ps -q | grep -q .; do sleep 1; done   # -> waits containers to stop
systemctl stop crio
```


# 3) Creating backup datadir

```shell
tar czf /var/tmp/backup/var.tgz \
    --exclude='/var/tmp/*' \
    --exclude='/var/lib/log/*' \
    --exclude='/var/log/*' \
    --exclude='/var/lib/containers/*' \
    --exclude='/var/lib/kubelet/pods/*' \
    --exclude='/var/lib/cni/bin/*' \
    --selinux \
    /var

ostree admin config-diff | awk '{print "/etc/" $2}' | xargs tar czf /var/tmp/backup/etc.tgz --selinux
rpm-ostree status -v --json > /var/tmp/backup/rpm-ostree.json
cp -v /etc/machine-config-daemon/currentconfig /var/tmp/backup/mco-currentconfig.json
ostree commit --branch ${backup_tag} /var/tmp/backup
```


# 4) Encapsulating and pushing backup OCI

```shell
ostree container encapsulate $backup_tag registry:$backup_refspec --repo /ostree/repo --label ostree.bootable=true
```


# 5) Encapsulating and pushing base OCI

```shell
base_commit=$(rpm-ostree status -v --json | jq -r '.deployments[] | select(.booted == true).checksum')
ostree container encapsulate $base_commit registry:$base_refspec --repo /ostree/repo --label ostree.bootable=true
```

```shell
# If there's a parent to that commit, also encapsulate it
if [[ "$(rpm-ostree status -v --json | jq -r '.deployments[] | select(.booted == true)| has("base-checksum")')" == "true" ]]; then
    echo "Parent commit found for base, encapsulating and pushing OCI"
    parent_commit=$(rpm-ostree status -v --json | jq -r '.deployments[] | select(.booted == true)."base-checksum"')
    ostree container encapsulate $parent_commit registry:$parent_refspec --repo /ostree/repo --label ostree.bootable=true
fi
```
