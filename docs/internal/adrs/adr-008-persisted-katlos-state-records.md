# ADR-008: Persisted KatlOS state uses self-describing record envelopes

Status: proposed.

Date: 2026-06-21.

## Context

Installed KatlOS systems keep node state under `/var/lib/katl`. That state
survives immutable root updates, A/B boot selection, failed upgrades, repair
boots, and rollback to a previous runtime root.

That makes persisted state a stronger contract than user-authored config or
compiled installer inputs. A new KatlOS runtime must be able to read older
state, and a previous known-good runtime may need to boot after a failed trial
upgrade where newer code has already touched `/var/lib/katl`.

The current code has several durable JSON records:

```text
/var/lib/katl/generations/<id>/spec.json
/var/lib/katl/generations/<id>/status.json
/var/lib/katl/generations/<id>/config-apply-status.json
/var/lib/katl/boot/selection.json
/var/lib/katl/install/status.json
/var/lib/katl/operations/<id>/record.json
/var/lib/katl/operations/<id>/journal/<seq>.json
/var/lib/katl/cluster/intent.json
/var/lib/katl/config-requests/...
```

Some records currently use `apiVersion` and `kind`, some use
`schemaVersion`, and some rely mostly on Go structs and path context. That is
enough for scaffolding, but it is not a durable installed-system compatibility
policy.

Katl should not turn node-local OS state into Kubernetes-style resources just
because KatlOS runs Kubernetes. The files need to be self-describing and easy
to decode, but they do not need Kubernetes object semantics such as
`metadata`, universal `spec`/`status`, namespaces, resource versions, managed
fields, admission, watches, or reconciliation.

## Decision

Katl persisted state files use a Katl-native self-describing record envelope.

The v0.1 persisted format remains JSON. JSON is the default for node-local
state because recovery and debugging should work with standard tools such as
`cat`, `jq`, and rescue media without requiring a Katl binary that can still run
on the damaged system.

Every durable record file has common top-level envelope fields followed by one
record-specific payload:

```json
{
  "recordType": "katl.generation.spec",
  "recordVersion": 1,
  "writtenBy": {
    "katlVersion": "0.1.0",
    "runtimeInterface": "katlos.runtime.v1"
  },
  "writtenAt": "2026-06-21T10:00:00Z",
  "payload": {
    "generationID": "gen-0",
    "runtimeVersion": "0.1.0"
  }
}
```

The required envelope fields are:

```text
recordType
  Stable Katl-owned discriminator for the record body, such as
  katl.generation.spec, katl.generation.status, katl.boot.selection,
  katl.operation.record, or katl.install.status.

recordVersion
  Positive integer local to recordType. Version 2 of one record type does not
  imply version 2 of any other record type.

payload
  The record-specific body. Its schema is selected by recordType and
  recordVersion.
```

The optional common diagnostic fields are:

```text
writtenBy.katlVersion
writtenBy.runtimeInterface
writtenAt
```

Record bodies must use fields that fit their local purpose. Katl must not force
every record into `metadata`, `spec`, and `status`. Immutable desired-selection
records and mutable observation records stay as separate files when they have
different write, digest, or rollback rules.

For example, generation selection and generation status are two different
records:

```json
{
  "recordType": "katl.generation.spec",
  "recordVersion": 1,
  "payload": {
    "generationID": "gen-0",
    "root": {},
    "boot": {},
    "sysexts": [],
    "confexts": []
  }
}
```

```json
{
  "recordType": "katl.generation.status",
  "recordVersion": 1,
  "payload": {
    "generationID": "gen-0",
    "specDigest": "sha256:...",
    "commitState": "committed",
    "bootState": "good",
    "healthState": "healthy"
  }
}
```

The file path remains part of validation, not the only way to identify the
record. A copied record is still self-describing. When a record is loaded from
its normal Katl path, path context and payload fields must agree. For example,
`/var/lib/katl/generations/gen-0/spec.json` must contain a
`katl.generation.spec` record whose payload names generation `gen-0`.

## Encoding And Protobuf

Katl uses protobuf for the `katlc` agent API and may use protobuf schema
definitions for code generation later, but v0.1 persisted node state is JSON on
disk.

Binary protobuf is not the default on-disk format for recovery-critical files.
It weakens direct operator inspection during repair, rescue boots, and bug
reports. If Katl later stores high-volume or non-recovery-critical data as
binary protobuf, it must provide an inspection tool and an explicit reason.

Using protobuf does not remove the compatibility work. Katl would still need to
define record type dispatch, field-number reservation, unknown-field handling,
mutation safety, rollback compatibility, canonical digests, and migrations.

## Compatibility Policy

Reading a record must be a two-stage operation:

```text
decode the envelope
dispatch on recordType and recordVersion
decode payload with the selected versioned decoder
validate path context, digests, and semantic constraints
```

Katl must not silently rewrite a record just because it was read. Any change to
an existing persisted record is either a normal state mutation for that record
or an explicit migration.

Record versioning rules:

```text
adding, removing, renaming, or changing the meaning of a payload field requires
  a new recordVersion unless the field lives in a specifically documented
  extension map
recordVersion is local to recordType
field names within a recordVersion are stable
enum values are strings and new values require reader behavior to be defined
IDs, digests, sizes, and timestamps are strings or integers, not floats
timestamps are RFC3339 UTC strings
digests are strings such as sha256:<hex>
canonical bytes for digesting are defined per record type and version
```

Readers must not drop unknown persisted state during mutation. For supported
record versions, unknown payload fields are rejected unless that record version
explicitly defines an extension or annotations map. Newer record versions are
rejected with a recovery-safe diagnostic unless the current binary has a
decoder for that version.

Rollback-sensitive records require an additional compatibility gate. A trial
runtime must not write a record version that the previous known-good runtime
cannot read well enough to select rollback or report repair unless the
operation explicitly declares rollback compatibility broken and has a tested
repair path.

Migrations are explicit operations, not incidental reads:

```text
validate current records
write a migration plan
capture rollback or repair evidence
write new records atomically
record the operation outcome
prove old/new behavior with fixtures and VM tests when rollback is affected
```

## Record Inventory

The first persisted record types should cover the existing durable files:

```text
katl.generation.spec
katl.generation.status
katl.generation.config-apply-status
katl.boot.selection
katl.install.status
katl.operation.record
katl.operation.journal-event
katl.cluster.intent
katl.config-request.decision
```

The record inventory is the authoritative list of installed-system disk
contracts. User-authored source config, Katl config bundles, compiled per-node
install material, artifact metadata, and the `katlc` protobuf API are separate
contracts with separate compatibility policies.

## Consequences

KatlOS state becomes self-describing without adopting Kubernetes API object
semantics. Operators and developers can inspect records directly, while Go code
gets deterministic envelope dispatch before decoding record bodies.

The project must migrate existing persisted structs away from ad hoc
`apiVersion`/`kind` and one-off `schemaVersion` fields into a common envelope.
That migration should happen before v0.1 declares an installed-system state
contract stable.

Compatibility tests need to become release artifacts. Each released
recordType/recordVersion pair needs fixtures that current code can read and
validate. Rollback-sensitive changes need VM coverage that proves a failed
trial upgrade can still reach a usable previous known-good generation or a
clear repair path.

## Open Questions

The following details need follow-up design or implementation decisions:

```text
exact recordType strings for all existing persisted files
whether writtenBy is required before v0.1 or remains diagnostic-only
whether canonical digests cover the whole envelope or only selected payload
  bytes for each record type
how much old-version decoding is needed before v0.1 has real release history
where migration plans and migration operation records live
whether bundle manifests should reuse this envelope or use OCI media-type
  identity only
whether protobuf schema definitions should become the source for generated Go
  structs while preserving JSON on disk
```
