# Generation Metadata Model

This decision defines the minimum generation record Katl needs for first
install and later A/B updates.

## Decision

Katl stores generation records under:

```text
/var/lib/katl/generations/<generation-id>/metadata.json
```

Each record selects one complete bootable generation: root slot, root artifact,
UKI, sysext set, generated confext set, and kernel command line. The selected
artifacts are activated and rolled back together, but they do not have to share
one product version. A generation can combine a KatlOS runtime root version with
an independently versioned Kubernetes sysext as long as the compatibility
metadata says that pair is supported. Rollback must switch the whole record
rather than independently switching root, sysext, and confext state.

The first implementation stores mutable boot and health status fields in the
same `metadata.json` file as the generation selection. The selection fields are
immutable after generation creation; only the status fields may be updated by
boot health, rollback, or explicit repair tooling. A later schema can split
immutable generation spec and mutable status into separate files without
changing the selection model.

## Required Fields

The first record schema is `katl.dev/v1alpha1`, `GenerationRecord`.

Required fields:

| Field | Purpose |
| --- | --- |
| `generationID` | Stable generation directory name under `/var/lib/katl/generations` |
| `runtimeVersion` | Human/runtime version used for compatibility checks and diagnostics |
| `root.slot` | Selected root slot, initially `root-a`; later `root-b` during A/B updates |
| `root.partitionUUID` | PARTUUID used by boot entries for the selected root partition |
| `root.runtimeArtifactSHA256` | Digest of the runtime root artifact written into the slot |
| `boot.ukiPath` | Installed UKI path selected with this generation |
| `sysexts[]` | Sysext name, generation-scoped path, activation path, digest, artifact version, payload version such as Kubernetes version, architecture, and compatibility metadata |
| `confexts[]` | Generated confext name, path, activation path, digest, and compatibility metadata |
| `kernelCommandLine[]` | Kernel arguments selected for this generation |
| `createdAt` | Generation creation timestamp |
| `bootState` | Mutable pending, trying, good, failed, or superseded boot state |
| `healthState` | Mutable unknown, healthy, unhealthy, or deferred runtime health state |

The Go scaffold in `internal/installer/generation` implements this initial
record shape and deterministic content identifiers for generated trees.

## Generation 0

First install creates generation 0, the post-install KatlOS baseline:

```text
/var/lib/katl/generations/<generation-id>/
  manifest.json
  metadata.json
  confext/
  sysext/
```

The installer writes the runtime artifact to `root-a`, records the selected
root PARTUUID and runtime artifact digest, records generated confext metadata,
and marks the generation:

```text
bootState: pending
healthState: unknown
```

Runtime health completion later marks generation 0 good. Generation 0 must boot
the installed runtime, mount writable state, expose operator access, store the
user-supplied cluster intent from the install manifest, and provide the
Katl/systemd wiring needed to accept later node-local operations. It is not
Kubernetes-capable, does not activate Kubernetes binaries, and does not run
`kubeadm init` or `kubeadm join`.

Generation 0 metadata must not list a Kubernetes sysext unless that sysext is
actually selected and active for generation 0, which is not the day-one model.
Bundled Kubernetes sysexts from the verified KatlOS image are install artifacts
or later generation inputs, not generation 0 selected host state.

The explicit cluster bootstrap or join operation asks `katlc` to validate the
stored intent and create a later generation, commonly described as generation 1.
That candidate generation selects the Kubernetes sysext, rendered kubeadm-ready
configuration, kubelet/containerd wiring, and `/etc/kubernetes` projection.

For first bootstrap or join, generation 1 selects the bundled Kubernetes sysext
whose payload version exactly matches the install manifest, for example manifest
version `1.36.1` selecting `katl-kube-1.36.1.sysext`. The generation 1 record
stores the selected sysext path, activation path, digest, artifact version,
payload version, architecture, and compatibility metadata. It remains a candidate
or deferred-health generation until kubeadm succeeds and local health checks
pass; only then can it be committed.

The first install path does not need inactive-slot rollback because there is no
previous installed generation.

Boot attempt and health state transitions are defined in
`docs/internal/boot-health-semantics.md`.

## Status Mutation

Only these fields are mutable after the generation is created:

```text
bootState
healthState
```

All other fields describe the selected root, UKI, command line, sysext set, and
generated confext set. Those fields must not be changed in place during a normal
update. A new desired runtime state gets a new generation directory.

`bootState` records boot trial status only. It is not the live configuration
apply phase. `healthState` records generation boot health. Live apply progress,
diagnostics, rollback attempts, and external mutation boundaries belong in an
operation record such as `config-apply-status.json` until the generation later
boots and reaches `katl-boot-complete.target`.

Valid boot transitions are:

```text
pending -> trying -> good
pending | trying -> failed
good -> superseded
```

A runtime config generation created for `live` starts as `pending` and
`unknown`. If live activation succeeds, its apply status may become active, but
the generation is not known-good until a boot health promotion marks it
`good` and `healthy`. A rollback target that is already `good` or `superseded`
stays known-good; selecting it for rollback must not erase that status.

Explicit repair tooling may update only `bootState` and `healthState` in an
existing generation record, and only through a durable operation record that
records previous values, new values, reason, and diagnostics. Repair tooling
must not change root slot, PARTUUID, UKI path, kernel command line, sysext
selection, or confext selection in place. If those fields are wrong or missing,
Katl must create a new generation, roll back to an existing valid generation, or
refuse.

## Updates

Updates create a new generation directory before switching boot selection. A new
generation may change the runtime root, the Kubernetes sysext, the generated
confext, or any supported combination of those artifacts. Runtime-root updates
write the inactive root slot. Sysext-only or confext-only updates may reuse the
current root slot and root artifact digest while selecting new extension content.
KatlOS-only updates may keep the existing Kubernetes sysext when that sysext is
compatible with the new runtime root. Kubernetes-only updates may keep the
existing KatlOS runtime root when the new sysext is compatible with it.

A Kubernetes upgrade operation may stage a candidate generation before every
service consumes it. That does not create an intermediate generation with mixed
Kubernetes tooling. The record still selects the target Kubernetes sysext as one
complete post-operation state; operation metadata controls when kubelet is
allowed to consume that payload. A candidate Kubernetes upgrade generation must
remain `healthState: unknown` or deferred until the kubeadm phase and planned
kubelet restart have completed and local health checks pass.

Katl uses `systemd-sysupdate` as the default resource transfer and staging
primitive for KatlOS runtime root and UKI updates, but the Katl generation
record remains authoritative for the complete selected runtime state. The
mapping is defined in
`docs/internal/systemd-sysupdate-update-decision.md`.

Generation activation should use systemd-native functions where practical:
systemd-boot for boot target selection, systemd-sysext and systemd-confext for
extension activation, native mount units for state projections, tmpfiles for
Katl-owned directory preparation, and boot health targets for known-good
promotion. Generation metadata is the Katl coordination layer around those
native mechanisms.

Runtime configuration changes are confext-only generations unless they are
combined with an explicit root or sysext update. The apply-mode decision for
those generated confext changes is recorded in
`docs/internal/adrs/adr-002-live-and-next-boot-config-apply-modes.md`.

Those runtime changes start as Katl configuration applied to the installed node.
The node renders the generated confext locally and writes a new generation
record that selects the rendered confext together with the current or updated
sysext set. User-supplied runtime input does not directly name generated confext
activation paths or mutate the sysext activation set outside generation
metadata.

`katlc` and KatlOS runtime services create a generation record only after
validation accepts the requested config. Unknown domains, unsupported fields,
unsupported sysext selection requests, and unsupported apply modes do not get
placeholder generation records; they produce rejected request status only.

Before a candidate generation is made bootable, Katl must validate the selected
runtime root, UKI, sysexts, and confexts as one compatibility set. The generation
record stores the exact artifact digests and versions that were validated, then
asks the boot selector to try that record.

Rollback returns to the previous generation record and therefore restores:

```text
root slot
UKI path
kernel command line
sysext activation set
confext activation set
```

Rollback selection rules are defined in
`docs/internal/rollback-selection-rules.md`.

## Deferred Fields

The first model intentionally defers:

```text
TPM measured boot state
verity metadata for generated confext images
per-file confext manifests
multi-architecture root type details
signed update metadata envelopes
boot counting integration details
operator-facing release notes
```

Those fields can be added in a later API version without changing the core rule
that a generation record selects the complete runtime state as one unit.
