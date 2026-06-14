# Stacked Etcd Bootstrap And Data Policy

Status: current decision.

This decision defines the greenfield control-plane etcd policy for Katl
clusters bootstrapped with kubeadm. It covers the initial bootstrap and data
handling boundary. Kubernetes sysext upgrade, kubeadm upgrade, and day-2 etcd
version behavior are separate upgrade decisions.

## Decision

The greenfield target is kubeadm stacked etcd.

Each control-plane node runs the etcd static pod managed by kubeadm. Katl does
not introduce an external etcd cluster, an etcd operator, or a separate etcd
lifecycle engine for the first home-cluster rebuild path.

External etcd remains out of scope until there is a concrete operational need
and a separate test matrix for external member health, credentials, backup,
restore, upgrade, and disaster recovery.

## Ownership Boundary

Katl owns:

```text
install-time storage planning for /var and optional /var/lib/etcd placement
systemd mount ordering before kubeadm-ready handoff
validated kubeadm input rendering under /etc/katl/kubeadm
node readiness checks before an operator-run bootstrap action starts
redacted diagnostics for bootstrap and recovery failures
```

Kubeadm and Kubernetes own:

```text
etcd static pod manifests under /etc/kubernetes/manifests
etcd member creation and removal semantics
etcd data contents under /var/lib/etcd
certificates, kubeconfigs, and uploaded certificate material
kubeadm upgrade and control-plane reconfiguration behavior
```

The operator owns:

```text
when to initialize the cluster
when to join additional control-plane nodes
when to take or restore etcd snapshots
whether a failed or stale member should be repaired or rebuilt
```

Katl may provide thin helper commands for these operator actions, but they must
remain kubeadm-aware actions rather than hidden effects of install, boot, or
normal confext activation.

## Data Placement

The default etcd data path is `/var/lib/etcd` on the Katl writable state
partition mounted at `/var`.

An optional Katl-owned `KATL_ETCD` partition may be mounted at `/var/lib/etcd`
when the install storage plan explicitly requests it. That partition is still
persistent node state, not a generation artifact. It is not an `extraDisks`
mount and must be planned as part of the root-disk storage layout so the
installer can reject ambiguous or unsafe placement.

Etcd data is outside Katl generation rollback:

```text
KatlOS root slot rollback
  does not roll back /var/lib/etcd

Kubernetes sysext or confext rollback
  does not roll back /var/lib/etcd

generated kubeadm input changes
  do not mutate live etcd data until an explicit kubeadm-aware action runs
```

`/etc/kubernetes` is also persistent projected state and must be treated with
the same care. A rebuilt root generation must never assume that rolling back
root, sysext, or confext content rewinds kubeadm output or etcd state.

## Greenfield Rebuild Semantics

A greenfield Katl rebuild is allowed to discard existing etcd state only when
the operator chooses a destructive reinstall or otherwise confirms that the
previous Kubernetes cluster state is not being preserved.

Rules:

```text
destructive reinstall of the target disk
  discards local /var/lib/etcd and /etc/kubernetes state on that disk

non-destructive repair or update
  must not delete, reformat, or reinterpret /var/lib/etcd

detected existing etcd data without an explicit destructive path
  refuse or require an explicit recovery workflow

automatic snapshot restore during greenfield install
  out of scope for the first implementation
```

If preserving a previous cluster matters, the operator must take an explicit
etcd snapshot before destructive work starts and restore it through a
kubeadm/etcd-aware recovery workflow. Katl should not infer that an old etcd
directory can be reused after a new greenfield install unless the recovery
workflow proves member identity, certificates, peer URLs, and Kubernetes
version compatibility.

Etcd recovery operation shapes are deferred. `ReplaceEtcdMember` and
`RestoreEtcdSnapshot` require explicit operator intent, member identity checks,
certificate compatibility checks, Kubernetes version compatibility checks, and
redacted diagnostics before any mutation. Until those operations exist and have
gates, Katl must refuse stale or partial etcd recovery cases instead of
attempting cleanup implicitly.

## Bootstrap Ordering

Three-control-plane stacked-etcd bootstrap is serial.

Required order:

```text
1. install every target node and wait for generation 0 installed-runtime health
2. select exactly one init control-plane node
3. ask `katlc` on the init node to create and activate its Kubernetes-capable
   candidate generation, then wait for katl-kubeadm-ready.target
4. run kubeadm init on the selected node
5. wait for API readiness and local etcd health on the init node
6. create or upload the control-plane join material with a certificate key
7. ask `katlc` on the second control-plane node to create and activate its
   Kubernetes-capable candidate generation, then wait for katl-kubeadm-ready.target
8. join the second control-plane node
9. wait for API readiness, node visibility, static pod health, and etcd member
   health
10. ask `katlc` on the third control-plane node to create and activate its
    Kubernetes-capable candidate generation, then wait for katl-kubeadm-ready.target
11. join the third control-plane node
12. wait for API readiness, node visibility, static pod health, etcd member
   health, and quorum
13. prepare and join workers, verify the cluster bootstrap result, write
    kubeconfig/status, commit successful candidate generations, and exit
```

Additional control-plane joins must not run in parallel. A failed join can leave
partial kubeadm output, certificates, static pod manifests, or an etcd member
record behind. Retrying the same node requires an explicit cleanup path. If a
member was added to the cluster before the join failed, recovery must remove
the failed member or prove it is healthy before retrying.

The bootstrap helper must never respond to a failed init by running
`kubeadm init` on another node against the same partially initialized cluster.
That is an explicit operator recovery decision.

Each kubeadm init or join step updates the bootstrap run record as a
`BootstrapCluster` or `JoinCluster` attempt. Failed control-plane joins must stop
the coordinator run and require explicit retry or repair; Katl must not
automatically keep reconciling membership.

Certificate-key handling must be explicit and redacted in normal logs, run
records, diagnostics, and artifacts.

## Quorum Rules

The target home-cluster control plane uses three stacked-etcd members. A healthy
cluster can tolerate one unavailable member.

Operational rules:

```text
maintain an odd number of voting etcd members for the intended steady state
avoid planned restarts or updates of more than one etcd member at a time
before joining a new control-plane member, verify the current members are
  healthy
after joining a member, verify the member list and endpoint status before the
  next join
do not proceed with worker joins or declare bootstrap complete if etcd health is
  unknown for a multi-control-plane scenario
```

Single-node control-plane smokes may prove kubeadm and static pod readiness, but
they do not prove quorum behavior.

## Rollback Limits

Katl root and sysext rollbacks are node-local artifact rollbacks. They do not
reverse kubeadm output or etcd contents.

After any kubeadm action or Kubernetes upgrade mutates etcd data, rollback has
these limits:

```text
rolling back the KatlOS root slot may restore host binaries and systemd units,
  but leaves etcd at the newer data state

rolling back the Kubernetes sysext may select older kubeadm, kubelet, kubectl,
  and CRI helper binaries, but leaves etcd at the newer data state

rolling back generated kubeadm input may restore older desired config files, but
  does not undo live Kubernetes objects, static pod manifests, or etcd schema
  changes
```

Upgrade tooling must therefore treat etcd snapshots and Kubernetes version skew
as explicit gates. Once etcd data or Kubernetes control-plane state has moved
forward, an automatic Katl generation rollback is not a complete cluster
rollback. The operator may need a kubeadm-aware recovery or a snapshot restore
instead.

The central host rollback boundary is defined in
`docs/internal/rollback-selection-rules.md`. Host rollback must not remove an
etcd member, restore an etcd snapshot, or claim quorum repair. Etcd recovery
remains a separate explicit operation.

## Validation Gates

The greenfield stacked-etcd path needs these gates before it is considered
ready for multi-control-plane use:

```text
single-node kubeadm API smoke
  proves /var, /etc/kubernetes, /var/lib/etcd, containerd, kubelet, kubeadm, and
  the API server work on one control-plane node

storage and ordering checks
  prove /var/lib/etcd is persistent state and any optional dedicated partition
  is mounted before kubeadm-ready handoff

three-control-plane bootstrap smoke
  runs serial init, second control-plane join, third control-plane join, and
  verifies API readiness after each step

etcd health checks
  verify etcd endpoint status and member list after init and after each
  control-plane join

snapshot gate, when preservation is claimed
  verifies snapshot creation, snapshot status, restricted artifact handling, and
  a documented restore path before destructive rebuild work; post-bootstrap
  snapshots are validation artifacts unless a restore workflow explicitly uses
  them

failed join recovery gate
  simulates an interrupted or failed second control-plane join and proves the
  helper either performs a tested cleanup/retry path or refuses with actionable
  diagnostics

rollback gate
  proves Katl root/sysext rollback does not claim to roll back etcd data and
  reports when kubeadm/Kubernetes state requires explicit operator recovery
```

Until those gates exist, Katl may support single-node control-plane bootstrap
and worker joins while refusing additional control-plane joins with a clear
unsupported message.

## Upgrade Constraints

The Kubernetes sysext and kubeadm upgrade design must account for the stacked
etcd policy:

```text
etcd data lives outside Katl generations
minor-version changes need kubeadm and Kubernetes skew validation before action
upgrade plans need an explicit etcd snapshot stance
control-plane node updates should be serialized and health-gated
rollback messaging must distinguish node artifact rollback from cluster data
  rollback
```

Those constraints belong in the upgrade-specific design before Katl claims
initialized multi-control-plane cluster upgrade or rollback support.
