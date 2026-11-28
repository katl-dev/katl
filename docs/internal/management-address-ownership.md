# Management address ownership

This document records who decides where a node is, whether it moved, and what
a reader should do when a management check fails. Issue #1107 tracks the
consolidation; each remaining duplicate below names the sub-issue that removes
it.

## Invariant

No command states or acts on a conclusion about where a node is, whether it
moved, or what its management trust is, until one owner has checked the
address the command is about to use. Conclusions come only from that owner's
typed result: where the address came from, the classified evidence of the
call, and the identity the endpoint reported over the configured credentials.
A difference between configuration files, or a failed TLS handshake, can
produce a conditional hint. Neither produces a refusal, destructive advice or
a trust remedy on its own.

## The owner

The owner is `cmd/katlctl/node_address.go`, with the failure explainer in
`cmd/katlctl/management_confirmation.go`. It has three stages, and each is
built only from the one before.

1. A candidate is the address a command will use for one node: its endpoint,
   the node's `management.address`, its role, where the endpoint came from
   (ClusterConfig, `--endpoint`, or an endpoint the same run already
   confirmed) and what the saved context records for the node.
   `resolveCandidate` builds it from the saved context as read from disk. A
   candidate is bound when the saved context records its endpoint for the
   node. A caller cannot assert binding. Every `--endpoint` is unverified,
   including one equal to the saved endpoint; #1112 decides that case.
2. A verified address is built only by `checkAddress`. It reads the node's
   status at the candidate's endpoint with the configured credentials and
   requires the reported node name. For an `--endpoint`, it also requires the
   installation the saved context records, when it records one. A ClusterConfig
   address keeps accepting a reinstalled node, which `context save` records.
   `checkAddress` owns the failure text, with the binding taken from the
   candidate.
3. A move is built only by `verified.move()`. It exists when the verified
   endpoint's host differs from the `management.address` host. Ports are
   never compared, so a forwarded port on the same host is not a move, for
   any role. Policies that refuse a move take one: `cluster apply` refuses a
   control-plane move and a move under `--mode live` through `move.refuse`.

`cluster apply --endpoint` checks the address on both management paths before
any move decision. With secrets the ClusterConfig names, or trusted-network
management, the check runs inside the refresh, which returns the verified
address of every node it observed. With mTLS and no secrets reference, the
check uses the saved credentials before the configuration is activated, as
`context rebind` does.

`TestAddressAdviceHasOneOwner` in `cmd/katlctl/address_ownership_test.go`
enforces this outside the owner files, in `cmd/katlctl` and `internal/katlc`.
It fails on an address or trust advice fragment, on a composite literal of
`candidate`, `verified`, `move` or `managementCheck`, and on a call to
`transport.ClassifyManagementEvidence`. Its allowlist only shrinks, because an
entry that no longer matches also fails the test.

## Remaining duplicates

| Site | Sub-issue |
| --- | --- |
| Binding literals in `context rebind`, the refresh and the enable-mtls saved-context refresh; the enable-mtls preflight's second binding derivation; `savedEndpoint`'s separate file read | #1112 |
| Stale-address and moved-node hints (`addressHintApplies`, `readdressHint`, `movedHint`), which the owner still selects with the helpers' own rules | #1113 |
| The pinned-kubelet worker refusal, and the pre-apply status failure in `activateClusterConfig` | #1114 |
| `node reboot`'s desired endpoint, `warnPendingReaddress` and the reboot outcome advice | #1115 |
| Identity-mismatch wordings in enable-mtls, rotation, the boot wait, the transport and bootstrap | #1116 |
| The session path for single-node commands and cluster status | #1117 |
| The zero-address escape hatch for trust-changing steps, and rotation's already-rotated check | #1108 |
| Reverse lookup of a node from an endpoint string | #1118 |
| Advice in `internal/katlc/transport` | #1119 |
| Extracting the owner into its own package and closing the allowlist | #1120 |

## Legitimately different sites

| Site | Reason |
| --- | --- |
| Trust-changing steps with a zero address in rotation, recovery and enable-mtls | The step's outcome is unknown, so the prior trust fixes the explanation and the command names its own step. #1108 replaces the zero value with a typed step. |
| SSH identity checks in rotation and recovery | A different channel with its own trust root, the pinned SSH host key. |
| Evidence classified for control flow only (shutdown wait, rotation, enable-mtls, `contextEndpointStatus`) | They decide whether to continue and give no advice. |
| Input validation of addresses | They say a required address is missing or malformed and claim nothing about a node. |
| Rename and role refusals in `cluster apply` | Decided from a status read after the node is verified. |
| Missing local credentials | Workstation file state, not node evidence. |
| The interrupted-trial fallback of `node status` | Gated on a typed observation and then verified with the enable-mtls secrets. |
| The installer handoff address, the Kubernetes API endpoint wait and the boot wait | Another service, another endpoint, or a target already bound by a verified status. |
| The `cluster apply` report's "still answers at" | Printed only after this run verified the node at its current address and built the change from a move. |
| Command usage examples and recovery progress text | They name the command itself or the authority it reads over SSH. |
