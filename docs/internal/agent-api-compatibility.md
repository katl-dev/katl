# Agent API compatibility

The `KatlcAgent` gRPC API connects workstation clients to installed KatlOS
nodes. Beginning with the first stable KatlOS release, a newer `katlctl` must
remain able to perform supported management operations against nodes from the
current stable release series and the two preceding stable release series.
Features introduced later can require a newer node; the client must report that
requirement or use an explicitly supported, narrower behavior.

## Version and retirement window

A stable release series is a distinct `YYYY.M` release line. Patch releases in
the same line do not advance the compatibility window. Pre-releases, including
betas, do not advance it either.

If a method or operation kind is deprecated in stable series N, keep it working
throughout N and the next two published stable series. It can be removed no
earlier than the third subsequent stable series, after supported clients have
migrated. For example, if N, N+1, and N+2 are the first three published stable
series, the earliest removal is N+3. A skipped calendar month does not count
as a release.
Contracts deprecated before the first stable release use that first stable
series as N. Patch releases cannot remove a contract or narrow its accepted
behavior.

The same window applies to request fields, response fields, operation kinds,
error codes, and behavior that a supported client relies on. Deprecation is a
notice to migrate, not permission to change the old contract in place. Record
the deprecation and earliest removal series in release notes and the protocol
definition before removing anything.

## Evolve the wire protocol

- Add an operation kind when behavior within `SubmitOperation` requires an old
  agent to understand new request fields, enforce a new safety rule, or return a
  result the client needs before acting. Advertise the kind in
  `NodeStatus.supported_operation_kinds` so a client can select it before
  submission. Use a new RPC method when the feature does not fit the operation
  model.
- Keep the old kind's accepted requests and response meaning intact for its
  retirement window. Additive fields are safe on an existing kind only when
  ignoring them cannot change the intended outcome.
- A client may select an older kind only when it can preserve the
  operator's requested behavior. Otherwise it must stop with guidance. Never
  retry a mutation after an ambiguous transport failure: use operation identity
  and status to establish whether the first request was accepted.
- Test the old contract and the new contract independently, including a client
  talking to an agent built before the new kind existed. Exercise planning,
  submission, and the resulting operation through the public interface.

## Host upgrade transition

`host-upgrade-v2` owns target-aware host upgrade planning and combined OS and
configuration upgrades on `SubmitOperation`. It uses the original
`SubmitOperationRequest` envelope kind. The `host-upgrade` operation kind with
that envelope retains its source-only semantics for existing clients. This
legacy request contract is deprecated as of the first stable series and remains
supported through its retirement window. The shipped beta.16 agent also
accepts `HostUpgradeRequestV2` as an envelope kind with
`host-upgrade`; this remains a transitional beta path.

`katlctl` selects `host-upgrade-v2` when the node advertises it. Otherwise it
tries the beta.16 envelope kind. Only the older agent's exact request-kind
rejection selects the original source-only plan. A source-only plan cannot
promise target-image validation, and a combined configuration upgrade cannot
use it. Future changes to required `host-upgrade-v2` semantics need a new
operation kind and the same retirement process.

For a plain host upgrade, agents that advertise `host-upgrade-handoff` run the
target release's generation planner before staging the inactive slot. A client
selects that kind when available. Combined `--apply-config` upgrades continue
to use `host-upgrade-v2` until configuration and external extension inputs are
part of the target preparation contract. The client must describe that
limitation if the older kind cannot represent the requested configuration.

## Image and persisted-state compatibility

Beginning with the first stable series, a target image must support host
upgrades from the current and two preceding published stable series. Its
preparation program must run on each supported source kernel and produce a
generation that both the target runtime and the source's rollback path can
read. The boot envelope, planner invocation, generation core, boot selection,
and rollback-sensitive state are stable contracts for that window. Additive
descriptive image metadata may be ignored by old agents. A new required boot
capability needs an explicit version check and a pre-mutation error that names
the minimum source release.

Target-specific generation data belongs in separate versioned attachments.
Do not add fields to the stable generation core that an old source must
understand to validate, publish, list, or roll back a candidate. Test each
supported source release with a frozen agent and the target image through
`katlctl` planning, upgrade, failed preparation, trial failure, rollback,
repeat boot, and rollforward. Patch releases cannot narrow an existing path.
Pre-releases do not advance this window; their release notes must state the
supported upgrade paths and any bridge needed from older betas. See
[ADR-016](adrs/adr-016-opaque-host-upgrade-handoff.md) for the ownership model.

This policy does not extend the compatibility guarantee to every
`v1alpha1` configuration document or arbitrary persisted-state changes.
