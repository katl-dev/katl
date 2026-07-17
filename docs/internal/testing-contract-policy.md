# Testing Contract Policy

Katl tests protect operator-visible behavior and durable product boundaries. A
test should not make the current Fedora closure, mkosi invocation, workflow
layout, documentation prose, or an internal file arrangement into a product
contract merely because that arrangement exists today.

## Exactness Budget

Use exact assertions for:

- destructive-action safety and disk state transitions;
- artifact digests, sizes, identities, and internal consistency;
- secret handling, authentication, redaction, and trust boundaries;
- Katl-owned interfaces consumed across an artifact or process boundary;
- immutable-root and persistent-state semantics;
- explicitly supported serialized formats; and
- operator-visible lifecycle outcomes and recovery classifications.

Prefer semantic assertions for generated configuration. Parse JSON, YAML, and
systemd configuration when structure matters. Exercise scripts and commands
through their inputs and outputs. Do not read implementation source and require
literal snippets, command spelling, line ordering, workflow topology, or prose.

## Distribution Inputs

Fedora's signed stable repositories are normal moving build inputs. Builds
record the selected release, repositories, resolved NEVRAs, and artifact
digests in resource and release evidence. That inventory describes what was
built; it does not reject a build because a transitive package changed.

Pin a package only for a documented compatibility or security reason. A true
reproducible-build claim requires retaining repository metadata and exact RPM
bytes; a committed list of NEVRAs is not such a claim. Optional strict package
verification may remain available for expert workflows without becoming part
of the routine Katl journey.

## Artifact Gates

Artifact checks verify checksums, metadata, Katl-owned executables and services,
state-layout interfaces, test/build payload exclusion, and capabilities that
cannot be deferred safely. They do not freeze compression tuning, every Fedora
binary path, the full kernel module inventory, or generated service directive
text. VM journeys prove that the shipped artifacts boot, install, expose
operator access, update, recover, and bootstrap Kubernetes.

## Compatibility

Static fixtures prove current readers and writers agree. They become backward-
compatibility fixtures only after Katl explicitly supports that persisted or
published version. Experimental `v1alpha1` formats may change together with
their fixtures; tests must not silently create a compatibility promise that the
support policy rejects.
