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

To create an installation ISO out of your Single Node OpenShift (SNO) OCI seed image, run the following command from your workstation:

```shell
export LCA_IMAGE=$(oc get deployment -n openshift-lifecycle-agent lifecycle-agent-controller-manager -o jsonpath='{.spec.template.spec.containers[?(@.name=="manager")].image}')
export SEED_IMAGE=quay.io/${MY_REPO_ID}/${MY_REPO}:${MY_TAG}
export SEED_VERSION=4.14.6
export WORKDIR=~/ibi-iso-workdir/
mkdir ${WORKDIR}
export AUTH_FILE=/path/to/seed-image-pull-secret.json
export PS_FILE=/path/to/release-pull-secret.json
export SSH_PUBLIC_KEY=~/.ssh/id_rsa.pub
export IBI_INSTALLATION_DISK=/dev/sda


ib-cli create-iso --installation-disk ${IBI_INSTALLATION_DISK} \
  --lca-image ${LCA_IMAGE} \
  --seed-image ${SEED_IMAGE} \
  --seed-version ${SEED_VERSION} \
  --auth-file ${AUTH_FILE} \
  --pullsecret-file ${PS_FILE} \
  --ssh-public-key-file ${SSH_PUBLIC_KEY} \
  --dir ./${WORKDIR}

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

Notice that the `--rhcos-live-iso` and the `--lca-image` flags are optional, if not provided the tool will use the defaults.
