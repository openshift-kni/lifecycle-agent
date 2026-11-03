# Lifecycle Agent Operator

[![Go Report Card](https://goreportcard.com/badge/github.com/openshift-kni/lifecycle-agent)](https://goreportcard.com/report/github.com/openshift-kni/lifecycle-agent)
[![Go Reference](https://pkg.go.dev/badge/github.com/openshift-kni/lifecycle-agent.svg)](https://pkg.go.dev/github.com/openshift-kni/lifecycle-agent)
[![License Apache](https://img.shields.io/github/license/openshift-kni/lifecycle-agent)](https://opensource.org/licenses/Apache-2.0)

## Overview

The Lifecycle Agent (LCA) Operator provides local, on-cluster lifecycle management for
[Single Node OpenShift (SNO)](https://docs.openshift.com/container-platform/latest/installing/installing_sno/install-sno-preparing-to-install-sno.html)
clusters. It performs image-based upgrades by pivoting between OSTree stateroots, enabling
fast, reliable major-version upgrades with automatic rollback on failure. A seed image
captured from a reference SNO is used to build the target stateroot, and OADP/Velero
backup-restore preserves application and platform state across the upgrade.

## Key Features

- **Image-based upgrades** via OSTree stateroot pivoting
- **Seed image generation** from a running SNO cluster
- **OADP-based backup/restore** of cluster and application state
- **IP configuration management** during upgrades
- **Automatic rollback** on upgrade failure or timeout
- **Container image pre-caching** to reduce upgrade downtime
- **USB image scaffolding** through `lca-cli create-usb`, which assembles bootable
  USB media for the image-based install workflow

## Custom Resource Definitions

All CRDs are cluster-scoped singletons under `lca.openshift.io/v1`:

| CRD | Singleton Name | Description |
|-----|---------------|-------------|
| `ImageBasedUpgrade` | `upgrade` | Manages the upgrade lifecycle through stages |
| `SeedGenerator` | `seedimage` | Triggers seed image creation from a running SNO |
| `IPConfig` | `ipconfig` | Manages IP configuration changes during upgrades |

## Upgrade Lifecycle

The `ImageBasedUpgrade` controller implements a stage-based state machine:

**Idle** → **Prep** → **Upgrade** → **Rollback**

- **Idle** — Default state; cleanup of previous upgrade artifacts
- **Prep** — Pulls the seed image, sets up a new OSTree stateroot, and optionally pre-caches container images
- **Upgrade** — Takes OADP backups, pivots to the new stateroot, restores backups, and reconfigures the cluster
- **Rollback** — Reverts to the previous stateroot if the upgrade fails or is manually triggered

## Getting Started

### Prerequisites

- Go 1.26.3 or newer (see `go.mod`)
- Access to an OpenShift SNO cluster
- `oc` or `kubectl` configured with cluster-admin privileges

### Build

```bash
make build       # Build the operator binary (bin/manager)
make cli-build   # Build the on-node CLI binary (bin/lca-cli)
```

### Deploy

```bash
make install deploy IMG=<image>   # Install CRDs and deploy the operator
make bundle-run                   # Deploy via OLM
```

### Test & Lint

```bash
make unittest    # Run unit tests with coverage
make ci-job      # Full CI pipeline (generate, fmt, vet, lint, test)
```

## Documentation

- [Image-Based Upgrade](docs/image-based-upgrade.md)
- [Seed Image Generation](docs/seed-image-generation.md)
- [Backup/Restore with OADP](docs/backuprestore-with-oadp.md)
- [Post-Pivot Configuration](docs/post-pivot-configuration.md)
- [IP Configuration](docs/ip-configuration.md)
- [Troubleshooting](docs/troubleshooting.md)
- [Examples](docs/examples.md)
- [Must-Gather](docs/must-gather.md)
- [USB Image Scaffolding and Manifest](lca-cli/README.md#creating-usb-media)
- [create-usb p3 config tree](docs/create-usb.md)
- [Disconnected IBI Durability](docs/disconnected-ibi-durability.md)

## Contributing

See [DEVELOPING.md](DEVELOPING.md) for development setup, testing, and contribution guidelines.

## License

This project is licensed under the Apache License 2.0 — see the [LICENSE](LICENSE) file for details.
