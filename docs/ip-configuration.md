# IP configuration flow

This document describes the **IP configuration flow** implemented by Lifecycle Agent (LCA) for **Single-Node OpenShift (SNO)** clusters. It includes:

- **Motivation**
- **Stage-based API**
- **Implementation details** (stateroots, reboot, pre-pivot/post-pivot)
- **Protection / rollback mechanisms**
- **Debugging guidance**

## Agenda

- [General overview](#general-overview)
- [Primary use cases](#primary-use-cases)
  - [Disaster recovery / DRP](#disaster-recovery--drp)
  - [Rehoming / site migration](#rehoming--site-migration)
- [Solution overview](#solution-overview)
  - [What’s in scope](#whats-in-scope)
  - [Design constraints and requirements (current implementation)](#design-constraints-and-requirements-current-implementation)
  - [Stages and high-level state machine](#stages-and-high-level-state-machine)
- [Implementation details](#implementation-details)
  - [Key components](#key-components)
  - [Two stateroots, one reboot](#two-stateroots-one-reboot)
  - [What recert does in this flow](#what-recert-does-in-this-flow)
  - [Networking specifics](#networking-specifics)
  - [DNSMasq integration (SNO)](#dnsmasq-integration-sno)
- [User guide](#user-guide)
  - [Prerequisites](#prerequisites)
  - [API reference](#api-reference)
  - [How to run an IP change (happy path)](#how-to-run-an-ip-change-happy-path)
  - [Protection mechanisms](#protection-mechanisms)
  - [Debugging and troubleshooting](#debugging-and-troubleshooting)
- [Notes and future directions](#notes-and-future-directions)

## General overview

Changing a SNO cluster’s “core networking” (node IP(s), machine network CIDR(s), default gateway, VLAN, DNS settings) is high-risk because it affects:

- **Host networking** (NetworkManager/OVS bridge `br-ex`, default routes, resolv.conf/DNSMasq behavior)
- **Cluster configuration** (node internal IPs, install-config networks, certificates/SANs and other IP‑anchored state)

LCA provides a **Kubernetes-native, stage-driven workflow** using a singleton Cluster-scoped CR:

- **Kind**: `IPConfig`
- **Name**: `ipconfig` (enforced singleton)
- **Short name**: `ipc`

The workflow is designed to be **reversible** while in-progress, and to require only the minimum disruption necessary:

- **Two stateroots** (old rollback target + new target stateroot)
- **One reboot** for the IP change flow (the pivot into the new stateroot)

## Primary use cases

## Disaster recovery / DR

After a site outage or major connectivity change, a SNO may need to move to a different uplink and:

- change IP address and/or machine network CIDR
- change default gateway
- change VLAN ID on the external uplink (`br-ex` path)
- update DNS servers
- optionally change dual-stack DNS resolution preference via DNS response filtering

## Rehoming / site migration

During network rehoming or consolidation, SNO clusters may be migrated to a new L2/L3 environment (new prefix/gateway/VLAN) with a deterministic, reversible process and minimal downtime.

## Solution overview

## What’s in scope

The `IPConfig` spec supports:

- **IPv4 and/or IPv6 address changes** (SNO single-stack or dual-stack)
- **Machine network CIDR changes**
- **Default gateway changes**
- **DNS server list** (ordered, IPv4 and/or IPv6)
- **Optional VLAN ID** on the `br-ex` uplink path
- **Optional DNS response filtering** on dual-stack clusters (filter out A or AAAA records)
- **Automatic rollback configuration**

## Design constraints and requirements

## Cluster requirements

- **SNO only** (controller validates there is exactly one master node)
- Cluster must be stable enough for health checks (unless explicitly skipped)
- A baseline DNSMasq MachineConfig must exist on the cluster, automatically installed
  by any SNO installed directly or idirectly by Assisted Installer (ABI, MCE, etc.) (validated)

## Networking topology constraints (current supported scenario)

The API/implementation is designed to be extensible, but the currently supported and validated scenario assumes:

- **One NIC**: a single uplink NIC is used (no bonding / no link aggregation).
- **One VLAN**: at most one VLAN ID is configured on the `br-ex` uplink path.
- **Dual-stack SNO**: exactly **one IPv4** and **one IPv6** address are provided/used for the node.
- **Static host networking (DR only)**: for disaster-recovery scenarios, the host networking is assumed to be statically configured (no DHCP).
- **No overlapping routes with the default gateway**: ensure the resulting routing table does not introduce routes that overlap/conflict with the default route via the configured gateway(s).
- **No proxy adjustments**: the IP configuration flow does **not** update cluster proxy configuration to match new proxy.

## API / spec constraints

- **Singleton**: `metadata.name` must be `ipconfig`.
- **Spec is mutable only while Idle**: the CRD blocks changing:
  - `spec.ipv4`, `spec.ipv6`, `spec.dnsServers`, `spec.dnsFilterOutFamily`, `spec.vlanID`, `spec.autoRollbackOnFailure`
  unless the old object is Idle (Idle condition `True`).
- **Stage transitions must be valid**: `spec.stage` may be changed only to a value listed in `status.validNextStages`.
  - This is enforced **server-side** by the CRD (XValidation). If `status.validNextStages` is missing, it indicates that **no transitions are currently allowed**.
  - If a stage transition request is invalid (or is otherwise not currently allowed), it is treated as an **InvalidTransition** (not a stage failure). Once a valid transition is requested, the controller clears the invalid-transition status and proceeds.
- **IP family compatibility**:
  - You can’t set `spec.ipv4` if the cluster is IPv6-only.
  - You can’t set `spec.ipv6` if the cluster is IPv4-only.
  - `dnsFilterOutFamily` is supported only on **dual-stack** clusters.
    - Setting `spec.dnsFilterOutFamily` to `ipv4` or `ipv6` is only allowed when the cluster is *observably* dual-stack via status (both `status.ipv4.address` and `status.ipv6.address` are present).
    - `spec.dnsFilterOutFamily: none` (or leaving it unset/empty) is always allowed.
  - `dnsServers` entries must match cluster families (no IPv4 DNS server on IPv6-only, etc.).
- **Address-change coupling** (current limitation):
  - For each family, changing **gateway** or **machineNetwork** without changing **address** is **not supported**.
  - Changing `dnsServers` without changing at least one IP address is **not supported**.
- **CIDR consistency**:
  - Address must be inside its `machineNetwork`.
  - Gateway must be inside its `machineNetwork` (IPv6 allows link-local gateways which are not in the `machineNetwork`).

## Stages and high-level state machine

`spec.stage` drives the controller; the controller publishes `status.conditions` and `status.validNextStages` to guide safe transitions.

- **Idle**
  - Initial and final stage.
  - Runs health checks (unless skipped via `lca.openshift.io/ipconfig-skip-pre-configuration-cluster-health-checks`).
  - Performs cleanup (including removing unbooted stateroots and LCA workspace).
  - Resets conditions back to a clean Idle state.
  - Resets `status.rollbackAvailabilityExpiration` back to empty/zero.
  - **Important**: transitioning to Idle after a successful Config is the **finalization point**; it will remove rollback ability

- **Config**
  - Executes the configuration flow:
    - **pre-pivot**: prepare a new stateroot + write configs + reboot into the new stateroot
    - **post-pivot**: edits the node and cluster, wait for cluster to stabilize (unless skipped via `lca.openshift.io/ipconfig-skip-post-configuration-cluster-health-checks`) and verify `status` matches `spec`

- **Rollback**
  - Reboots back into the previous stateroot if it is still available, and completes rollback once the system stabilizes.

## Implementation details

## Key components

- **Kubernetes API**
  - `IPConfig` CRD and singleton CR (`ipconfig`)
  - `IPConfig` controller (`controllers/ipc_*`)

- **Host-side CLI**
  - `lca-cli ip-config pre-pivot` (runs on old stateroot, prepares new stateroot and reboots)
  - `lca-cli ip-config post-pivot` (runs in new stateroot via systemd, applies nmstate + runs recert)
  - `lca-cli ip-config rollback` (runs on request to reboot to old stateroot)

- **Systemd units**
  - `lca-ipconfig-pre-pivot` (transient `systemd-run` unit created by the controller to run pre-pivot)
  - `ip-configuration.service` (oneshot unit in the new stateroot that runs `lca-cli ip-config post-pivot`)
  - `lca-init-monitor.service` (watchdog; triggers rollback after timeout if not disabled)
  - `lca-ipconfig-rollback` (transient `systemd-run` unit created by the controller to run rollback)

## Two stateroots, one reboot

The Config flow intentionally preserves the currently booted stateroot as a **rollback target** while preparing the new configuration in a **new stateroot**.

At a high level:

1. **Controller pre-pivot phase (old stateroot is booted)**
   - Validates request (SNO, DNSMasq MC exists, spec compatibility/constraints).
   - Writes config files under `/var/lib/lca/workspace/` for the CLI.
   - Writes auto-rollback config for init-monitor and post-pivot failure rollback.
   - Schedules `lca-cli ip-config pre-pivot` via `systemd-run` (`lca-ipconfig-pre-pivot`).

1. **`lca-cli ip-config pre-pivot` (old stateroot)**
   - Collects required cluster inputs for recert (install-config, kubeconfig crypto, ingress CN, current node IPs).
   - Generates:
     - `recert_config.json` (for the new IPs/machine networks)
     - `nmstate.yaml` for `br-ex` (IP(s), routes/gateways, DNS servers, optional VLAN)
   - Stops core cluster services (to safely copy dynamic state).
   - Deploys a new stateroot identical to the current one (kernel args preserved), copies required state, and sets it as the default deployment (when supported).
   - Writes the generated files into the **new stateroot** workspace and adjusts dnsmasq overrides/filter config.
   - Removes known stale OVN/OVS/Multus files so they can regenerate after IP change.
   - Installs/enables `ip-configuration.service` and (optionally) `lca-init-monitor.service` in the **new stateroot**.
   - Reboots into the new stateroot.

1. **`ip-configuration.service` (new stateroot, post-reboot)**
   - Executes `lca-cli ip-config post-pivot`:
     - applies `nmstate.yaml` (`nmstatectl apply`)
     - runs recert using `recert_config.json` to rewrite cluster state/certs for the new IPs (see [What recert does in this flow](#what-recert-does-in-this-flow))
     - re-enables kubelet and waits for the API to come back
     - disables itself so it won’t re-run
   - On failure, may trigger **automatic rollback** (see below).

1. **Controller post-pivot phase (new stateroot is booted)**
   - Disables the init-monitor watchdog (when safe).
   - Waits for cluster stabilization (unless skipped), and checks that `status` matches the requested `spec`.
   - Marks Config complete and offers next stages via `status.validNextStages`.

## Networking specifics

- Host networking changes are applied through **NMState** on the `br-ex` path.
- The flow detects the **physical interface** attached to `br-ex` using `ovs-vsctl list-ports br-ex` (skipping patch ports) to generate the nmstate configuration.
- Default routes are derived from the provided gateways.
- DNS configuration uses an ordered list (`spec.dnsServers`) and optional dual-stack DNS response filtering (`spec.dnsFilterOutFamily`) implemented via a dnsmasq filter file.

## What recert does in this flow

In the IP configuration flow, **recert** is the component responsible for updating **cluster state and certificates** so the cluster can come up cleanly after the node IP(s) change.

- **What triggers it**
  - `ip-configuration.service` runs `lca-cli ip-config post-pivot`, which runs the recert “full flow” using the generated `recert_config.json`.

- **What inputs recert consumes**
  - `recert_config.json` written to the workspace during pre-pivot.
  - Cluster crypto material needed to regenerate crypto objects while keeping access functional (collected during pre-pivot into the workspace).
  - Install-config and ingress certificate CN (used to build correct replacement rules/config).

- **What recert does (high level)**
  - Runs as a container image (default pullspec is `quay.io/edge-infrastructure/recert:v0`, configurable via LCA) and is executed via `podman` with host directories mounted.
  - Brings up an **unauthenticated etcd** endpoint backed by the real etcd database to enable safe, offline edits during the maintenance window (see `lca-cli/ops/ops.go`).
  - Uses the configuration in `recert_config.json` to update IP-anchored cluster state and regenerate/adjust certificates so they match the new node IP(s) and machine network(s).

## DNSMasq integration (SNO)

The flow integrates with the Assisted Installer’s dnsmasq configuration:

- `SNO_DNSMASQ_IP_OVERRIDE` in `/etc/default/sno_dnsmasq_configuration_overrides` is updated in the new stateroot.
- Optional DNS filtering (dual-stack only) writes/removes a file at:
  - `/etc/dnsmasq.d/single-node-filter.conf`
  - Contents are managed by LCA and will be one of `filter-A` or `filter-AAAA` (or removed for `none`).

## User guide

## Prerequisites

- Your cluster is **SNO**.
- LCA is deployed and the `IPConfig` CRD exists.
- The singleton CR should exist:

```bash
oc get ipc ipconfig -o yaml
```

If it doesn’t exist, it is created automatically by LCA initialization logic.

## API reference

## Resource

- **apiVersion**: `lca.openshift.io/v1`
- **kind**: `IPConfig`
- **metadata.name**: `ipconfig` (required singleton)

## Spec fields (user-controlled)

- **`spec.stage`**: `Idle` | `Config` | `Rollback`

- **`spec.ipv4`** *(optional, omit for IPv6-only)*:
  - **`address`** *(required if ipv4 present)*: IPv4 address **without prefix** (e.g., `192.0.2.10`)
  - **`machineNetwork`** *(optional but typically required for changes)*: CIDR (e.g., `192.0.2.0/24`)
  - **`gateway`** *(optional)*: IPv4 gateway address (e.g., `192.0.2.1`)

- **`spec.ipv6`** *(optional, omit for IPv4-only)*:
  - **`address`** *(required if ipv6 present)*: IPv6 address **without prefix** (e.g., `2001:db8::10`)
  - **`machineNetwork`** *(optional but typically required for changes)*: CIDR (e.g., `2001:db8::/64`)
  - **`gateway`** *(optional)*: IPv6 gateway (link-local allowed)

- **`spec.dnsServers`** *(optional)*:
  - Ordered list of DNS server IPs (IPv4 and/or IPv6), e.g. `["192.0.2.53","2001:4860:4860::8888"]`

- **`spec.vlanID`** *(optional)*:
  - VLAN ID (minimum 1). If omitted/0, no VLAN is applied.

- **`spec.dnsFilterOutFamily`** *(optional; dual-stack clusters only)*:
  - `ipv4`: filter out A records (prefer IPv6)
  - `ipv6`: filter out AAAA records (prefer IPv4)
  - `none`: explicitly disable filtering (removes the managed dnsmasq filter file)
  - Note: setting it to `ipv4`/`ipv6` is only allowed when the cluster is observably dual-stack (both `status.ipv4.address` and `status.ipv6.address` are present).

- **`spec.autoRollbackOnFailure.initMonitorTimeoutSeconds`** *(optional)*:
  - Timeout for the init-monitor watchdog; `0` or unset uses default **1800s (30m)**.

## Status fields (observed)

- **`status.conditions`**: Kubernetes conditions used to reflect stage progress and errors.
  - In-progress condition types for IPConfig:
    - `Idle` (with `status: "False"` and reason `InProgress` while transitioning)
    - `ConfigInProgress`
    - `RollbackInProgress`
  - Completed condition types:
    - `ConfigCompleted`
    - `RollbackCompleted`

- **`status.validNextStages`**: controller-computed list of allowed next values for `spec.stage`.

- **`status.ipv4` / `status.ipv6`**: detected node IP + inferred machine network + default gateway.

- **`status.dnsServers`**: detected ordered DNS servers from host nmstate.

- **`status.vlanID`**: detected VLAN ID on `br-ex` path (if any).

- **`status.dnsFilterOutFamily`**: inferred active dnsmasq filter-out family (`ipv4`, `ipv6`, or `none`).

- **`status.history`**: timestamps for stage/phase progression (useful for auditing/debugging).

- **`status.rollbackAvailabilityExpiration`**: a timestamp indicating when rolling back may start
  to require **manual recovery** due to expired control plane / kubelet certificates in the
  rollback (unbooted) stateroot. This is best-effort computed during Config post-pivot (based on
  the earliest kubelet client/server certificate expiry, minus 30 minutes) and is reset when
  returning to `Idle`.

## Annotations (behavioral controls)

Common controller annotations:

- **`lca.openshift.io/trigger-reconcile`**: force a reconcile loop.
- **`lca.openshift.io/manual-cleanup-done`**: used to continue if automatic cleanup in Idle failed and the user has done manual cleanup.
- **`lca.openshift.io/ipconfig-skip-pre-configuration-cluster-health-checks`**: presence flag to skip **pre-configuration** health checks (used in Idle and before starting Config; use carefully and mostly for DR/lab).
- **`lca.openshift.io/ipconfig-skip-post-configuration-cluster-health-checks`**: presence flag to skip **post-configuration** stabilization health checks during Config (does not affect pre-pivot checks).

Recert image handling:

- **`lca.openshift.io/recert-image`**: override recert image.
- **`lca.openshift.io/recert-pull-secret`**: name of a Secret in `openshift-lifecycle-agent` namespace with `.dockerconfigjson` to pull recert image.
- **`lca.openshift.io/recert-image-cached`**: set by controller when it has successfully cached the recert image on the host.

Auto-rollback toggles:

- **`auto-rollback-on-failure.lca.openshift.io/ip-config-run: "Disabled"`**: disables automatic rollback on `ip-config post-pivot` failure.
- **`auto-rollback-on-failure.lca.openshift.io/init-monitor: "Disabled"`**: disables init-monitor timeout rollback.

## How to run an IP change (happy path)

1. **Inspect the current state**

```bash
oc get ipc ipconfig -o yaml
```

1. **While `spec.stage` is `Idle`, edit desired networking fields**

Example (dual-stack + VLAN + DNS list + DNS filtering):

```yaml
apiVersion: lca.openshift.io/v1
kind: IPConfig
metadata:
  name: ipconfig
spec:
  stage: Config
  ipv4:
    address: 192.0.2.10
    machineNetwork: 192.0.2.0/24
    gateway: 192.0.2.1
  ipv6:
    address: 2001:db8::10
    machineNetwork: 2001:db8::/64
    gateway: 2001:db8::1
  dnsServers:
  - 192.0.2.53
  - 2001:4860:4860::8888
  vlanID: 100
  dnsFilterOutFamily: ipv4
  autoRollbackOnFailure:
    initMonitorTimeoutSeconds: 1800
```

1. **Watch progress**

```bash
oc get ipc ipconfig -o wide
oc get ipc ipconfig -o yaml
```

Expect:

- controller sets Config “in progress”
- pre-pivot runs and triggers a reboot into the new stateroot
- `ip-configuration.service` runs post-pivot, applies networking + recert
- controller waits for stabilization and then marks Config completed
- `status.validNextStages` typically becomes `["Idle","Rollback"]` after a successful config

1. **Finalize or rollback**

- **Finalize (commit the change)**:
  - set `spec.stage: Idle`
  - controller will run cleanup and reset conditions
  - after this, rollback may no longer be available (old stateroot may be removed during Idle cleanup)

- **Rollback (revert to previous stateroot)**:
  - set `spec.stage: Rollback` (only when it is listed in `status.validNextStages`)
  - controller runs rollback and reboots into the unbooted stateroot
  - when completed, set `spec.stage: Idle` to clean up

## Protection mechanisms

The IP config flow has multiple safety nets:

## 1) Automatic rollback on post-pivot failure (code failure)

If `lca-cli ip-config post-pivot` fails (invoked by `ip-configuration.service`), it may trigger **automatic rollback** back to the unbooted stateroot, unless disabled via:

- `auto-rollback-on-failure.lca.openshift.io/ip-config-run: "Disabled"`

## 2) Init-monitor timeout rollback (watchdog)

If the IP config flow does not complete within the configured timeout, `lca-init-monitor.service` triggers rollback automatically (unless disabled via `auto-rollback-on-failure.lca.openshift.io/init-monitor: "Disabled"`).

Timeout is controlled by:

- `spec.autoRollbackOnFailure.initMonitorTimeoutSeconds` (default 1800 seconds)

## 3) Manual rollback stage

When available, the controller exposes `Rollback` in `status.validNextStages`, allowing an explicit rollback request (`spec.stage: Rollback`).

## 4) Persistence for “uncontrolled rollback” / crash scenarios

The controller exports IPConfig state to `/var/lib/lca/ipc.json` before starting the pre-pivot reboot, so that rollback logic can update status messaging even across restarts.

## Debugging and troubleshooting

## Inspect the CR and conditions

```bash
oc get ipc ipconfig -o yaml
```

Pay special attention to:

- `status.conditions[-1]` (latest state/reason/message)
- `status.validNextStages`
- `status.history` timestamps

## Controller logs (operator)

Look at the LCA operator/controller logs in the `openshift-lifecycle-agent` namespace.

## Node-side systemd services

These are the highest-signal logs for failures around the reboot boundary:

- **Pre-pivot (old stateroot)**
  - `lca-ipconfig-pre-pivot` (created by `systemd-run`)

- **Post-pivot (new stateroot)**
  - `ip-configuration.service` (runs `lca-cli ip-config post-pivot`)
  - `lca-init-monitor.service` (watchdog, if enabled)

- **Rollback (when requested)**
  - `lca-ipconfig-rollback` (created by `systemd-run`)

Typical commands (run on the node):

```bash
sudo journalctl -u lca-ipconfig-pre-pivot -b --no-pager
sudo journalctl -u ip-configuration.service -b --no-pager
sudo journalctl -u lca-init-monitor.service -b --no-pager
sudo journalctl -u lca-ipconfig-rollback -b --no-pager
```

## Useful on-node artifacts

These files are used to pass data across the reboot boundary:

- `/var/lib/lca/workspace/ip-config-pre-pivot.json`
- `/var/lib/lca/workspace/ip-config-post-pivot.json`
- `/var/lib/lca/workspace/nmstate.yaml`
- `/var/lib/lca/workspace/recert_config.json`
- `/var/lib/lca/workspace/recert-pull-secret.json` (if needed)
- `/var/lib/lca/workspace/ip-config-autorollback-config.json`
- `/var/lib/lca/ipc.json` (persistence for rollback/status continuity)

## Common failure patterns

- **Invalid transition**
  - `spec.stage` is not in `status.validNextStages`.
  - With the current CRD, this is typically rejected server-side with a validation error referencing `status.validNextStages`.

- **Spec cannot be changed**
  - You edited spec fields while the CR is not Idle (CRD validation rejects it).

- **Health checks never pass**
  - Investigate cluster health blockers. As a last resort (lab/DR), you can skip health checks using the presence-flag annotations:
    - `lca.openshift.io/ipconfig-skip-pre-configuration-cluster-health-checks: ""`
    - `lca.openshift.io/ipconfig-skip-post-configuration-cluster-health-checks: ""`

- **Post-pivot service failed**
  - Inspect `ip-configuration.service` logs; if auto-rollback is enabled, the node may revert to the old stateroot automatically.

## Notes and future directions

- The current implementation is intentionally conservative:
  - it requires changing gateway/machineNetwork only together with address changes
  - it uses a stateroot pivot and systemd-managed post-pivot execution
- Future enhancements may relax some limitations and/or improve ergonomics, but must preserve safety and rollback guarantees.
