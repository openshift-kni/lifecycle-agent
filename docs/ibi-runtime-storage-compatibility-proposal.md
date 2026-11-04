# Proposal: register the USB IBI durability store directly with CRI-O

Status: **minimum viable design; implemented, E2E not yet validated**.
The CRI-O registration path and LCA Podman command construction are implemented.
The remaining validation gates below must still be exercised on a supported host
before this is customer-ready.

## Scope

This proposal targets **self-contained USB IBI only**.

The additional durable store is not required to work for non-USB IBI, IBU,
connected, or install-only flows. Those flows remain out of scope, but they
must not be broken by this change.

## Problem

Self-contained USB IBI copies a validated, read-only `containers/storage` store
to:

```text
/var/lib/lca-ro-store
```

The previous implementation registered that store through:

```text
/etc/containers/storage.conf.d/50-lca-ro-store.conf
```

That mechanism does not work on the affected RHCOS host because the host Podman
binary uses an older `containers/storage` configuration loader that does not
discover files under `storage.conf.d`. Testing with LCA's newer vendored
`containers/storage` library is therefore not sufficient to prove host runtime
compatibility.

The store itself is valid. When the same host Podman binary is run with a
complete primary configuration that explicitly registers
`/var/lib/lca-ro-store`, all 122 exact references from the USB digest manifest
resolve locally, including recert, without pulls or imports. A CRI-O
configuration dump also consumed that explicit configuration.

What is missing is a registration path that the installed CRI-O and LCA's
Podman invocations actually use on the affected host.

## Minimum robust fix

### 1. Keep the existing ro-store handoff

Do not change the current store production path:

1. Copy the store from the USB media to the installed disk.
2. Validate its overlay format.
3. Validate every manifest image resolves through it.
4. Apply the correct SELinux label.
5. Atomically publish it at `/var/lib/lca-ro-store`.
6. Fail closed if the store is missing, incomplete, or cannot be validated.

The existing store handoff is already designed to be crash-safe and
customer-ready. The fix should replace only the broken registration mechanism,
not redesign store staging or validation.

The producer no longer stages a storage configuration artifact beside the store on p2.
The only runtime registration is the CRI-O drop-in written during prepare.

### 2. Register the store with CRI-O using a CRI-O drop-in

Write a CRI-O configuration drop-in to the installed deployment:

```text
/etc/crio/crio.conf.d/99-lca-ro-store.conf
```

Use this content:

```toml
[crio]
storage_option = [
  "imagestore=/var/lib/lca-ro-store",
]
```

Why this is the right minimum integration point:

- CRI-O supports configuration files under `/etc/crio/crio.conf.d`.
- CRI-O reads `storage_option` independently of the broken
  `containers/storage.conf.d` discovery path.
- `imagestore=/var/lib/lca-ro-store` is the storage-driver option that adds the
  path as a read-only additional image store.
- CRI-O merges `storage_option` values from drop-ins with the base
  configuration and removes duplicate entries, so this does not replace the
  platform's existing storage options.
- No MachineConfig, systemd environment file, or runtime service is required.

Write the drop-in only after the store is verified and published. Use an atomic
temporary-file-and-rename write. If the target already contains an unexpected
file with the same name, fail rather than overwrite it.

Because the drop-in is written to the installed deployment before first target
boot, CRI-O picks it up when the target starts. Prepare must not restart CRI-O
merely to apply it.

### 3. Make LCA-controlled Podman use the store explicitly

For the Podman commands LCA runs during self-contained USB IBI, prepend the
global storage option:

```text
--storage-opt imagestore=/var/lib/lca-ro-store
```

For example, recert must be invoked through the real LCA command path as:

```text
podman --storage-opt imagestore=/var/lib/lca-ro-store run --pull=never ...
```

This avoids a global `CONTAINERS_STORAGE_CONF` override and avoids changing
interactive administrator Podman behavior.

Add a small helper in `lca-cli/ops` that constructs Podman commands for the
USB IBI path with this option. Apply it to every LCA-controlled Podman
invocation that must resolve images from the durable store, especially recert.
Do not export the option globally through `/etc/environment`.

Post-pivot must not infer local-image-store mode from file existence alone.
Before enabling it, validate the exact CRI-O drop-in content and confirm the
durable store directory exists. A corrupt drop-in or missing store must fail
closed rather than silently reverting to connected-mode pulls.

### 4. Keep recert fail-closed

In self-contained USB IBI, recert must not silently fall back to a registry
pull.

Use Podman's explicit no-pull mode:

```text
--pull=never
```

If the selected image is not available locally through the effective storage
configuration, fail with a useful error. Do not treat image absence,
configuration errors, and SELinux errors as the same failure.

Connected-mode fallback remains intentionally out of scope for this change.

### 5. Do not add a new API or MCO workflow

This fix intentionally does **not** add:

- a `SeedGenerator` API field,
- seed capability metadata,
- a controller-driven MachineConfig preparation phase,
- a reusable renderer,
- a systemd `EnvironmentFile`,
- dormant-mode machinery,
- or a new runtime service.

Those mechanisms are appropriate for a broader reusable-seed design, but they
are not required to make the USB IBI ro-store visible to CRI-O and LCA's
Podman invocations.

## Why this is enough

The existing ro-store is already complete and validated. The only missing
piece is runtime registration.

- CRI-O's supported configuration drop-in path is `/etc/crio/crio.conf.d`.
- The CRI-O drop-in adds `/var/lib/lca-ro-store` as an additional image store.
- CRI-O and rootful Podman use the same writable graphroot on RHCOS, so adding
  the same additional image store makes the closure visible to both.
- LCA's own Podman commands receive the same storage option explicitly.
- The fix is scoped to self-contained USB IBI and does not alter non-USB IBI,
  IBU, connected, or install-only flows.
- The store and configuration are published only after validation, preserving
  the current fail-closed behavior.

This is the smallest change that addresses the actual compatibility failure
without introducing a new platform integration surface.

## Compatibility with existing OpenShift configuration

OpenShift already uses `/etc/crio/crio.conf.d` as a supported CRI-O
configuration surface:

- The MCO writes `/etc/crio/crio.conf.d/00-default`.
- `ContainerRuntimeConfig` writes separate `01-ctrcfg-*` drop-ins for the
  fields it manages.
- The MCO config-drift monitor ignores files in that directory that are not
  declared by the current MachineConfig, so an additional unmanaged
  `99-lca-ro-store.conf` does not itself trigger drift.
- CRI-O merges `storage_option` values from the base configuration and
  drop-ins, so adding `imagestore=/var/lib/lca-ro-store` does not replace the
  platform's existing storage options.

The remaining compatibility risks are:

1. A customer-defined MachineConfig or custom CRI-O drop-in could own the same
   filename. The prepare step must fail if `/etc/crio/crio.conf.d/99-lca-ro-store.conf`
   already exists with unexpected content.
2. A future OpenShift release could begin managing that exact path. The upgrade
   gate must verify the file survives MCO reconciliation.
3. A custom CRI-O drop-in could set `storage_option`. CRI-O merges those values,
   but the combined effective configuration still needs to be validated on the
   supported host.

If those checks fail, the fallback is to deliver the same CRI-O drop-in through
an authoritative MachineConfig. Do not reintroduce the broader reusable-seed
design for this time-critical fix.

## Remaining validation gates

Before shipping, validate the following on the actual supported RHCOS build:

1. **CRI-O config drop-in**
   - Confirm CRI-O reads `/etc/crio/crio.conf.d/99-lca-ro-store.conf`.
   - Confirm the effective CRI-O configuration includes
     `imagestore=/var/lib/lca-ro-store`.
   - Confirm existing CRI-O storage options are preserved.

2. **CRI-O runtime recovery**
   - Restart CRI-O with registry access blocked.
   - Resolve every exact manifest reference locally.
   - Prove image lookup and container creation through the ro-store.
   - A configuration dump alone is not sufficient.

3. **Podman path**
   - Confirm CRI-O and rootful Podman use the same writable graphroot on the
     target.
   - Confirm the supported Podman version accepts
     `--storage-opt imagestore=/var/lib/lca-ro-store`.
   - Exercise the actual LCA host-namespace Podman command path.
   - Confirm recert resolves locally with `--pull=never`.

4. **SELinux**
   - Confirm CRI-O can read the store with its applied label.
   - Confirm the LCA Podman path can read the same store.

5. **MCO and upgrade behavior**
   - Confirm MCO does not remove or overwrite the CRI-O drop-in during target
     reconciliation or upgrade.
   - If this validation fails, the minimal fallback is to deliver the same
     CRI-O drop-in through an authoritative MachineConfig; do not reintroduce
     the broader reusable-seed design.

6. **Offline E2E**
   - Install on a disposable target with registry egress blocked.
   - Verify recert and post-pivot complete with no fallback pull.
   - Reboot and exercise the writable-image GC/recovery scenario without
     deleting anything from the ro-store.

## Proposed code slices

1. Replace the current `storage.conf.d` registration in
   `lca-cli/ibi-preparation/rostore.go` with the CRI-O drop-in described above.
2. Add a helper in `lca-cli/ops` that prepends
   `--storage-opt imagestore=/var/lib/lca-ro-store` to LCA-controlled Podman
   commands in the self-contained USB IBI path.
3. Add `--pull=never` to the self-contained recert invocation and distinguish
   local-image-missing errors from runtime/configuration errors.
4. Update unit and integration tests for:
   - CRI-O drop-in content and atomic publication,
   - conflict detection,
   - Podman command construction,
   - recert no-pull behavior,
   - missing-store failure handling.
5. Update the USB IBI durability and troubleshooting docs with the new
   registration path and diagnostic commands.

## Acceptance criteria

- Self-contained USB IBI media still produces a complete, validated ro-store.
- CRI-O starts with `/var/lib/lca-ro-store` registered as an additional image
  store.
- Every exact manifest reference resolves locally with registry access blocked.
- Recert runs with `--pull=never` and does not fall back to a pull.
- The store, CRI-O drop-in, and Podman command path are exercised on the real
  supported RHCOS host.
- Non-USB IBI, IBU, connected, and install-only flows remain unchanged.
- The fix does not add a new public API, MachineConfig workflow, or global
  storage override.
