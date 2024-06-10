# IB CLI

This application will assist users to easily create installation ISO from OCI seed image for the Image-Based installation (IBI) workflow, using
a simple CLI.

## What does this tool do?

The purpose of the `ib-cli` tool is to assist in the creation of IBI installation ISO, which are used later on for
installing baremetal nodes with SNO seed image during image based installation (IBI)

In that direction, the tool does the following at a high level:

- Download rhcos live-iso
- Create an ignition config that configures the live-ISO to do the following once a node is booted with the ISO:
  - Install rhcos to the installation disk
  - Mount the installation disk
  - Restore SNO from the seed image
  - Precache release container images to /var/lib/containers directory
- Embed the ignition into the live-ISO

### Building

Building the binary locally.

```shell
-> make cli-build
go mod tidy
Running go fmt
go fmt ./...
Running go vet
go vet ./...
go build -o bin/lca-cli main/lca-cli/main.go
go build -o bin/ib-cli main/ib-cli/main.go
```

> **Note:** The binary can be found in `./bin/ib-cli`.

### Running the tool's help

To see the tool's help on your local host, run the following command:

```shell
-> ./bin/ib-cli -h
ib-cli assists Image Based Install (IBI).

  Find more information at: https://github.com/openshift-kni/lifecycle-agent/blob/main/ib-cli/README.md

Usage:
  ib-cli [command]

Available Commands:
  completion  Generate the autocompletion script for the specified shell
  create-iso  Create installation ISO from OCI image.
  help        Help about any command

Flags:
  -h, --help       help for ib-cli
  -c, --no-color   Control colored output
  -v, --verbose    Display verbose logs

Use "ib-cli [command] --help" for more information about a command.
```

### Get the ib-cli binary

```shell
export LCA_IMAGE=$(oc get deployment -n openshift-lifecycle-agent lifecycle-agent-controller-manager -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')
podman run -it -v `pwd`:/data --entrypoint cp $(LCA_IMAGE) /usr/local/bin/ib-cli ./data/
```

### Create installation ISO

To create an installation ISO out of your Single Node OpenShift (SNO) OCI seed image, create `image-based-install-iso.yaml` configuration file inside working directory.

```yaml
seedImage: <seed image>
seedVersion: <seed version>
pullSecret: |
  <pull secret>
installationDisk: <disk name or disk id>
sshKey: |
  <ssh key>

# optional in order to change default base iso
rhcosLiveIso: "https://mirror.openshift.com/pub/openshift-v4/amd64/dependencies/rhcos/latest/rhcos-live.x86_64.iso"
```

After creating `image-based-install-iso.yaml` run:

```shell

ib-cli create-iso --dir <your working directory>

ib-cli assists Image Based Install (IBI).

  Find more information at: https://github.com/openshift-kni/lifecycle-agent/blob/main/ib-cli/README.md

INFO[2024-02-12 15:58:09] Installation ISO creation has started
INFO[2024-02-12 15:58:09] Creating IBI installation ISO
INFO[2024-02-12 15:58:09] Generating Ignition Config
INFO[2024-02-12 15:58:09] Executing podman with args [run -v ./ibi-iso-workdir:/data:rw,Z --rm quay.io/coreos/butane:release --pretty --strict -d /data /data/config.bu]
INFO[2024-02-12 15:58:10] Downloading live ISO
INFO[2024-02-12 15:59:24] Executing podman with args [run -v ./ibi-iso-workdir:/data:rw,Z quay.io/coreos/coreos-installer:latest iso ignition embed -i /data/ibi-ignition.json -o /data/rhcos-ibi.iso /data/rhcos-live.x86_64.iso]
INFO[2024-02-12 15:59:24] installation ISO created at: ibi-iso-workdir/rhcos-ibi.iso
INFO[2024-02-12 15:59:24] Installation ISO created successfully!
```

### Proxy Configuration

```yaml
proxy:
  httpProxy: "http://proxy.example.com:8080"
  httpsProxy: "http://proxy.example.com:8080"
  noProxy: "no_proxy.example.com"
```

### Mirror Registry

```yaml
imageDigestSources:
  - mirrors:
      - "<mirror_host_name>:<mirror_host_port>/ocp4/openshift4"
    source: "quay.io/openshift-release-dev/ocp-release"
  - mirrors:
      - "<mirror_host_name>:<mirror_host_port>/ocp4/openshift4"
      - "<mirror_host_name>:<mirror_host_port>/ocp4/openshift5"
    source: "quay.io/openshift-release-dev/ocp-v4.0-art-dev-5"
```

### Additional trusted bundle

```yaml
additionalTrustBundle: |
  PEM-encoded X.509 certificate bundle.
```

### Static network configuration

```yaml
nmStateConfig: |
  interfaces:
    - name: enp1s0
      type: ethernet
      state: up
      identifier: mac-address
      mac-address: <your mac>
      ipv4:
        enabled: true
        dhcp: false
        auto-dns: false
        address:
          - ip: 192.168.126.151
            prefix-length: 24
      ipv6:
        enabled: false
  dns-resolver:
    config:
      server:
        - 192.168.126.1
  routes:
    config:
      - destination: 0.0.0.0/0
        metric: 150
        next-hop-address: 192.168.126.1
        next-hop-interface: enp1s0
```

### Shutdown after installation

In order to shutdown node after installation add the following to `image-based-install-iso.yaml`:

```yaml
shutdown: true
```

### Image Precaching

By default, ib-cli will precache images and will fail in case image precaching didn't succeed.
In order to disable precaching add `precacheDisabled: true` to `image-based-install-iso.yaml`.
In order to run precaching in `best-effort` mode add `precacheBestEffort: true` flag.

### Additional flags

Can be found at the `ImageBasedInstallConfig` struct definition (link to [Go code](https://github.com/openshift-kni/lifecycle-agent/blob/main/api/ibiconfig/ibiconfig.go#L17)).
