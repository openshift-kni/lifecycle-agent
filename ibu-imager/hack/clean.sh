#!/bin/bash

sudo rm -rf /var/tmp/container_list.done \
    /var/tmp/backup  && \
sudo rm -f /usr/local/bin/prepare-installation-configuration.sh \
    /usr/local/bin/installation-configuration.sh && \
sudo systemctl disable installation-configuration.service && \
sudo systemctl disable prepare-installation-configuration.service && \
rm -f /etc/systemd/system/installation-configuration.service \
    /etc/systemd/system/prepare-installation-configuration.service && \
sudo podman rmi quay.io/alosadag/ibu-seed-sno0:oneimage --force && \
sudo systemctl enable --now kubelet && \
    sudo systemctl enable --now crio
