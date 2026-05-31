# Generation Metadata Model

This decision defines the minimum generation record Katl needs for first
install and later A/B updates.

## Decision

Katl stores generation records under:

```text
/var/lib/katl/generations/<generation-id>/metadata.json
```

Each record selects one complete bootable generation: root slot, root artifact,
UKI, sysext set, generated confext set, kernel command line, and health state.
Rollback must switch the whole record rather than independently switching root,
sysext, and confext state.

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
| `sysexts[]` | Sysext name, generation-scoped path, activation path, and digest |
| `confexts[]` | Generated confext name, path, activation path, digest, and compatibility metadata |
| `kernelCommandLine[]` | Kernel arguments selected for this generation |
| `createdAt` | Generation creation timestamp |
| `bootState` | Pending, trying, good, failed, or superseded boot state |
| `healthState` | Unknown, healthy, unhealthy, or deferred runtime health state |

The Go scaffold in `internal/installer/generation` implements this initial
record shape and deterministic content identifiers for generated trees.

## First Install

First install creates exactly one generation:

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

Runtime health completion later marks the same generation good. The first
install path does not need inactive-slot rollback because there is no previous
installed generation.

## Updates

Updates create a new generation directory before switching boot selection. The
runtime agent writes the inactive root slot, stages matching sysext/confext
sets, writes a new metadata record, and then asks the boot selector to try that
record.

Rollback returns to the previous generation record and therefore restores:

```text
root slot
UKI path
kernel command line
sysext activation set
confext activation set
```

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
