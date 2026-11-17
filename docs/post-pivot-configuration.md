# IBI post-pivot configuration

- [IBU/I post pivot configuration](#ibi-post-pivot-configuration)
  - [Overview](#overview)
  - [Network configuration](#network-configuration)
  - [Recertification flow](#recertification-flow)
  - [Offline reconfigure and the pre-recert clock gate](#offline-reconfigure-and-the-pre-recert-clock-gate)
  - [User specifications](#user-specifications)

## Overview

Post pivot configuration is the first thing running after node reboots into new stateroot.
It runs as part of [installation-configuration.service](../lca-cli/installation_configuration_files/services/installation-configuration.service)
It is on charge of list of operation that should occur in order for cluster to start:

- Perform networking configuration
- Recertification
- User specifications:
  - Apply new hostname
  - Apply new ip
  - Apply all user specific configurations that user provides as [seed reconfiguration](../api/seedreconfig/seedreconfig.go)
  - Applying extra manifests

## Mount configuration folder

There are 2 different ways to provide configuration folder:

1. Add it as part of stateroot configuration process as we do in IBU, should be placed in /opt/openshift
2. Provide it as block device with label "relocation-config" , in this case we will mount it.

Folder structures can be seen [prepare-installation-configuration.sh](../ibu-imager/installation_configuration_files/scripts/prepare-installation-configuration.sh
and will be described later on

## Network configuration

User can provide specific network configuration that he wants to be applied on the node as part of installation process.
Currently, we support nmconnection files that should be placed in /opt/openshift/network-configuration/system-connections

When the folder is already present at boot (IBU), [lca-restore-nmconnections.service](../lca-cli/installation_configuration_files/services/lca-restore-nmconnections.service)
copies them to the NetworkManager configuration folder before NetworkManager starts.
Otherwise NetworkManager auto-activates a default DHCP profile on the NIC and keeps it even after a restart.
Post pivot copies the files again and restarts NetworkManager. For a configuration folder provided later as a block device,
NetworkManager has already started by then, and the restart may keep a default DHCP connection it already activated.
Use NMState configuration (`raw_nm_state_config`) in that case, since post pivot applies it with `nmstatectl apply`.

## Recertification flow

Part of installation flow is recertification of the sno cluster, this process requires list of certificates provided by user.
List of files can be seen here [recert.go](../internal/recert/recert.go)
Those certificates currently are expected to be in /opt/openshift/certs folder.

## Offline reconfigure and the pre-recert clock gate

The self-contained USB-based IBI profile reconfigures the node entirely offline, from the
seed data already on the installed disk and the USB media. With the recert image precached
(mandatory for this profile), the reconfigure path up to and including recert contacts no
external endpoints: recert runs as a local `podman` container over local mounts, DNS is served
locally by dnsmasq, and chrony is never waited on for an external NTP sync. If the recert image
is somehow absent it now fails fast with a clear "must be precached" message rather than
stalling on a pull that cannot succeed offline.

Because recert mints certificates with validity windows derived from the system clock, a
grossly wrong clock (an unset or battery-dead RTC landing in 1970/2000, or a wildly future
date) would produce certificates that can block the cluster from becoming available — and
offline there is no NTP server to correct it. To prevent this, a **clock gate** runs
immediately before recert and verifies the clock is plausible. The gate applies only to the
self-contained profile (detected by the presence of the writable `ibi-status` partition) and
is a no-op once recert has already completed, so a converged node is never halted on a later
reboot.

The gate accepts the clock when **either**:

- a **trusted local time source** is configured and confirms synchronization before recert — a
  genuine external or on-link chrony source (`server`/`pool`/`peer` pointing at a real address,
  or a hardware `refclock`) that passes a bounded `chronyc waitsync`. A configured source that
  cannot be reached (the usual offline case) does not satisfy this path; sources backed by the
  RTC itself (`local stratum`, the `127.127.x.x` reference-clock pseudo-addresses, loopback, or
  `rtcsync`-only configs) are explicitly not trusted, so the gate can never be bypassed by the
  very clock it is meant to check; **or**
- the system clock falls within the plausible window `[seed_creation_time, seed_creation_time +
  max_seed_age]`. `seed_creation_time` is stamped into the seed at build time. `max_seed_age`
  defaults to **180 days** and is configurable via the `max_seed_age` field of the
  [seed reconfiguration](../api/seedreconfig/seedreconfig.go) (a Go duration string such as
  `4320h`); invalid, non-positive, or absurdly large values are rejected with a clear config
  error.

On the self-contained USB-based IBI profile both knobs are set **explicitly** through the
`create-usb` manifest's `siteConfig` (`chronyConfig` and `maxSeedAge`), which render into
`manifest.json`; the node never auto-trusts inherited host chrony configuration. Both are
validated at build time with the same rules the gate applies at runtime (shared via
[api/seedreconfig](../api/seedreconfig/clockgate_config.go)): a `chronyConfig` that declares no
trusted source, and an out-of-range `maxSeedAge`, fail the build rather than the node. See
[create-usb](./create-usb.md).

If neither holds, recert never runs: the gate writes a terminal `reconfigure.json` failure
record to the `ibi-status` partition (recording the observed time, the plausible window, and
which check failed) and halts the node **powered-on** via `emergency.target` so a technician can
diagnose and correct the clock. The terminal verdict is **idempotent**: an existing terminal
`reconfigure.json` (success or failure) is never overwritten, so a later boot with a corrected
clock passes the gate and runs recert, but it does **not** silently flip the recorded verdict.
To recover, clear `reconfigure.json` and re-enable the result-writer unit (see the convergence
verdict below and the [troubleshooting guide](./troubleshooting.md#the-reconfigure-result-reconfigurejson)).

**Accurate clock past the ceiling.** A node genuinely deployed more than `max_seed_age` after the
seed was built, with an accurate clock and no trusted local time source, will fail the gate —
without a trusted source the gate cannot distinguish "accurate clock, old seed" from "wrong
clock." Mitigate this by configuring a reachable local time source or by widening `max_seed_age`
for long-shelf deployments.

**Older seeds (compatibility limitation).** Seeds built before `seed_creation_time` existed carry
no timestamp. For those the gate applies only a degraded, lower-bound-only check derived from
the seed's control-plane certificate validity (it still rejects a grossly-past clock) and
otherwise proceeds with a warning. This degraded path does **not** provide the full plausibility
window and is not a substitute for a seed that carries `seed_creation_time`.

### The reconfigure convergence verdict

The clock gate decides only the pre-recert failure case. The **success** verdict — and the
post-recert timeout failure — is owned by a separate, single-writer **reconfigure
result-writer** so the long "operators Available" wait never blocks post-pivot. It runs as a
dedicated `lca-reconfigure-result-writer.service` systemd unit, **not** inside post-pivot:

- **Criterion.** Success is node `Ready` **and** all cluster operators converged (`Available`,
  not `Progressing`, not `Degraded`). The writer polls live cluster state (~every 30s), so it
  tolerates the `installation-configuration.service` restarts post-pivot goes through without
  racing them.
- **Timeout.** If the criterion is not met within the convergence timeout it writes a `failure`
  and halts the node **powered-on** via `emergency.target`. The timeout defaults to **60
  minutes** and is configurable via `siteConfig.reconfigureConvergenceTimeout` (create-usb). The
  deadline is anchored at **post-pivot start — after the pre-recert clock gate, so the anchor
  reflects any clock step the gate makes** — captured **write-once** under `/var/lib/lca` (which
  post-pivot cleanup never touches), so post-pivot retries never reset it.
- **Gating.** The unit is ordered `After=installation-configuration.service` (ordering only,
  never `Requires=`/`Requisite=`, because the `RemainAfterExit=no` post-pivot goes inactive on
  success) and `WantedBy=multi-user.target`. It runs with `TimeoutStartSec=0` so systemd never
  kills it before its own deadline. A single `ConditionPathExists` on the **durable handoff**
  (`/var/lib/lca/reconfigure-handoff.json`), plus an in-code **explicit self-contained profile**
  check, make it a clean no-op on IBU/install-only. The **success verdict** is additionally gated —
  in code, not by the unit condition — on the **post-pivot completion marker**
  (`/var/lib/lca/reconfigure-completed`, written only once recert and manifests are applied), so the
  writer never records a false success against the still-healthy seed cluster if post-pivot fails
  before recert completes. Startup is deliberately **not** gated on that marker: the writer must run
  to **enforce the deadline** even when post-pivot stalls or fails and never writes it — otherwise
  the node would hang instead of halting. The handoff is written *early* (right after the clock gate,
  before recert) to anchor the deadline, so it cannot double as the completion gate.
- **Pending-success durability.** The moment the writer observes the criterion it durably records a
  pending-success decision (`/var/lib/lca/reconfigure-success-pending`) *before* writing the terminal
  verdict to p4. If that p4 write fails and the node reboots past the write-once deadline, a later
  boot re-publishes the already-made success from this marker instead of re-deciding against
  possibly-transient live state — so a node that already converged can never be recorded as a
  failure by a failed write plus an ill-timed reboot.
- **Idempotence and self-disable.** After writing either terminal verdict the unit disables
  itself (best-effort — a failed disable never demotes a success or skips the halt). An existing
  terminal verdict is never overwritten, so a reboot that re-runs the unit is a no-op. The
  pre-recert clock-gate carve-out is the one case the result-writer never runs: the halt before
  recert prevents the node from reaching `multi-user.target`.

Recovery after a timeout failure requires **renewing the write-once convergence anchor** — so the
writer gets a fresh deadline and polls convergence again instead of re-failing on the expired
one — then clearing `reconfigure.json`, **restoring the node to `multi-user.target`** (a `failure`
isolates `emergency.target`, which stops the cluster services the node needs to converge), **and**
re-enabling the self-disabled unit. Automatic `installation-configuration.service` retries
deliberately keep the original deadline, so only this explicit operator recovery grants a new
anchor; see the
[troubleshooting guide](./troubleshooting.md#the-reconfigure-result-reconfigurejson) for the exact
steps.

## User specifications

We provide a way to specify a list of parameters that should be provided as json file in /opt/openshift/cluster-configuration/manifest.json
This file structure can be seen as struct in [SeedReconfiguration](../api/seedreconfig/seedreconfig.go)

### Hostname

Post pivot process is on charge of changing hostname for the node. User should provide new hostname that will be put into
/etc/hostname. When ocp cluster will start it will not have any nodes and new node with set hostname will be added.

### ClusterName

The desired cluster name for the cluster. Equivalent to install-config.yaml's clusterName.
This will replace the cluster name of the seed cluster.

### BaseDomain

The desired base domain for the cluster. Equivalent to install-config.yaml's baseDomain.
This will replace the base domain of the seed cluster.

#### ClusterName and BaseDomain fields

Recert will use those fields in order to generate new CN name in cluster certificates
and will change old one to new one in all relevant places.
Params can be seen here [recert.go](../internal/recert/recert.go)

### ClusterID

The desired cluster-ID. During an IBU, this is the ID of the original SNO.
During an IBI, this can either be empty, in which case a new cluster-ID will be generated by LCA, or it can be set to the ID of the
new cluster, in case one had to be pre-generated for some reason.

### Release registry

In order to set right release image registry in post pivot operation we need to get user release registry
that will be set in clusterversion release image param in case seed was created with another one.
