#!/bin/bash

sudo rm -rf /var/tmp/container_list.done \
    /var/tmp/backup  && \
sudo rm -f /usr/local/bin/prepare-installation-configuration.sh \
    /usr/local/bin/installation-configuration.sh && \
sudo systemctl disable installation-configuration.service && \
sudo systemctl disable prepare-installation-configuration.service && \
rm -f /etc/systemd/system/installation-configuration.service \
    /etc/systemd/system/prepare-installation-configuration.service && \
sudo systemctl enable --now kubelet
