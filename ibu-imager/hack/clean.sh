#!/bin/bash

# 1) cleanup backup dir
rm -rf /var/tmp/backup /var/tmp/checks

# 2) cleanup preparation scripts and services
rm -f /usr/local/bin/prepare-installation-configuration.sh /usr/local/bin/installation-configuration.sh
systemctl disable installation-configuration.service
systemctl disable prepare-installation-configuration.service
rm -f /etc/systemd/system/installation-configuration.service /etc/systemd/system/prepare-installation-configuration.service
rm -f /etc/systemd/system/installation-configuration.env

# 3) restore etcd
RECERT_IMAGE="quay.io/edge-infrastructure/recert:latest"
ETCD_IMAGE=$(jq -r '.spec.containers[] | select(.name == "etcd") | .image' </etc/kubernetes/manifests/etcd-pod.yaml)

podman run --name recert_etcd --detach --rm --network=host --privileged --replace --authfile /var/lib/kubelet/config.json --entrypoint etcd -v /var/lib/etcd:/store ${ETCD_IMAGE} --name editor --data-dir /store
podman run --name recert --rm --network=host --privileged --replace --authfile /var/lib/kubelet/config.json -v /etc/kubernetes:/kubernetes -v /var/lib/kubelet:/kubelet -v /etc/machine-config-daemon:/machine-config-daemon ${RECERT_IMAGE} --etcd-endpoint localhost:2379 --static-dir /kubernetes --static-dir /kubelet --static-dir /machine-config-daemon --extend-expiration

podman rm -f recert_etcd
podman rm -f recert

# 4) restore cluster services
rm -rf /var/lib/ovn-ic/etc/ovnkube-node-certs/* /etc/cni/multus/certs*
systemctl enable --now kubelet
