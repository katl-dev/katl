# ADR-006: Cluster Manifest Layering And Node Classes

Status: superseded before release by the v1alpha1 ClusterConfig contraction.

## Context

An earlier design introduced defaults, node classes, system-role defaults, and
per-node overrides. It optimized for schema flexibility before Katl had real
operator workflows demonstrating a need for that layering. It also exposed
compiler concepts and forced operators to understand merge precedence.

## Superseding Decision

ClusterConfig now has only shared `spec.defaults` and flat `spec.nodes[]`
entries. Katl selects role profiles internally from `systemRole`. Node classes,
system-role default layers, the `overrides` wrapper, and platform API endpoint
helpers are outside the supported v1alpha1 contract.

The governing rule is that a ClusterConfig value must represent a meaningful
choice by the cluster operator. Artifact selection, profile selection,
generation construction, credentials, and operation bookkeeping belong to Katl
or to the operation invoking it.

## Consequences

- The source document is shorter and has no merge-precedence ceremony.
- Strict decoding rejects the unshipped layering shapes and aliases.
- Hardware grouping can be reconsidered only when a concrete user workflow
  proves it useful.
- Internal compiled plans may retain richer structures where they help Katl;
  those structures are not operator API compatibility commitments.

The current contract is documented in
[`cluster-manifest-contract.md`](../cluster-manifest-contract.md).
