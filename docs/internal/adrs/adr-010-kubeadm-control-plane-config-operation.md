# ADR-010: Control-plane configuration changes use a narrow rolling kubeadm operation

Status: accepted.

Date: 2026-07-11.

## Context

Katl renders desired native kubeadm input under
`/etc/katl/kubeadm/<name>/config.yaml`, but normal generation activation must
not rewrite kubeadm-owned state. A running control plane has derived state in
the `kube-system/kubeadm-config` ConfigMap and in static Pod manifests under
`/etc/kubernetes/manifests`. Changing the desired file is therefore not the
same action as changing the live cluster.

Kubeadm has no generic reconfiguration transaction. It exposes individual
phases. In Kubernetes v1.36, `kubeadm init phase control-plane all --config`
generates the API server, controller manager, and scheduler static Pod
manifests, while `kubeadm init phase upload-config kubeadm --config` uploads the
`ClusterConfiguration` used by later join, reset, and upgrade operations.
Those phases can be composed safely only for a bounded field set and a serial
multi-node rollout.

The first KatlOS release needs one real control-plane configuration change. It
does not need a general wrapper over every kubeadm field. Broad support would
silently include endpoint, certificate, etcd, admission, feature-gate, host
volume, and networking changes whose safety and rollback procedures differ.

Upstream command references used by this decision:

- <https://kubernetes.io/docs/reference/setup-tools/kubeadm/generated/kubeadm_init/kubeadm_init_phase_control-plane_all/>
- <https://kubernetes.io/docs/reference/setup-tools/kubeadm/generated/kubeadm_init/kubeadm_init_phase_upload-config_kubeadm/>
- <https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-config/>
- <https://kubernetes.io/docs/reference/command-line-tools-reference/kube-apiserver/>
- <https://kubernetes.io/docs/reference/command-line-tools-reference/kube-controller-manager/>
- <https://kubernetes.io/docs/reference/command-line-tools-reference/kube-scheduler/>

## Decision

Katl supports a distinct `kubeadm-control-plane-config` operation. It is the
only v0.1 path that may make a desired control-plane configuration change live.
It is separate from normal confext apply and from `kubeadm-upgrade`.

The v0.1 operation supports only setting `profiling` to `false` in these
`ClusterConfiguration` fields:

```yaml
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  extraArgs:
    - name: profiling
      value: "false"
controllerManager:
  extraArgs:
    - name: profiling
      value: "false"
scheduler:
  extraArgs:
    - name: profiling
      value: "false"
```

An operation may change any non-empty subset of those three fields. Adding the
explicit false value is supported; removing it, setting it to true, repeating
the argument, or changing any other live kubeadm field is unsupported in v0.1.
This narrow allowlist proves rolling static-Pod reconfiguration without adding
new certificates, files, sockets, volumes, API dependencies, or etcd behavior.

The selected kubeadm API must be `kubeadm.k8s.io/v1beta4`. The active
Kubernetes payload version and digest remain unchanged for this operation. A
request that also changes the Kubernetes payload must use `kubeadm-upgrade` and
cannot be combined with this operation.

## Source Of Truth

Desired state is the selected `KubeadmConfig` in one committed Katl generation:

```text
desired generation ID
selected KubeadmConfig name
/etc/katl/kubeadm/<name>/config.yaml
canonical desired config SHA-256
```

The operation accepts only a config selected by the active generation. It does
not accept an arbitrary path or inline replacement YAML. Every participating
control-plane node must report the same cluster-wide desired
`ClusterConfiguration` digest after excluding node-local `InitConfiguration`
fields.

Live state is collected read-only from:

```text
kube-system/kubeadm-config ConfigMap
/etc/kubernetes/manifests/kube-apiserver.yaml
/etc/kubernetes/manifests/kube-controller-manager.yaml
/etc/kubernetes/manifests/kube-scheduler.yaml
active Kubernetes sysext metadata
running static Pod and component health
```

Katl canonicalizes the live ConfigMap and desired `ClusterConfiguration`
before comparing them. The planner must show the exact three supported field
paths that differ. Any other difference is `unsupported/manual`, not a partial
apply.

## Validation And Refusal Rules

Before accepting a rollout, `katlctl` and node-local `katlc` validate:

```text
exactly three intended control-plane nodes are present in explicit inventory
all nodes agree on cluster identity and stable control-plane endpoint
all nodes run the same Kubernetes payload version and digest
the desired generation is active and committed on every target node
the selected KubeadmConfig name and cluster-wide digest agree on every node
the live kubeadm ConfigMap digest matches the request's expected-live digest
the only desired/live differences are supported profiling=false additions
all three nodes are Ready and the stable API endpoint is healthy
all three stacked-etcd members are healthy and voting
no concurrent kubeadm-state operation owns a target node
an etcd snapshot record names its path, SHA-256, revision, member-list digest,
  creation time, source etcd version, and operator identity
```

The operation refuses:

```text
controlPlaneEndpoint, networking, kubernetesVersion, certificates, certSANs,
  certificate directories, image repository, DNS, proxy, encryption, or
  feature-gate changes
any etcd local/external setting or etcd static Pod patch
apiServer, controllerManager, or scheduler arguments other than profiling=false
extraEnvs, extraVolumes, arbitrary static Pod patches, or host-path changes
InitConfiguration, JoinConfiguration, KubeletConfiguration, or
  KubeProxyConfiguration changes
adding, removing, or replacing a control-plane or etcd member
combined Kubernetes payload and control-plane configuration changes
one-node or two-node execution presented as the three-control-plane release path
parallel node mutation, unknown quorum, an unhealthy API, or stale expected-live
  evidence
```

Katl may later expand the allowlist one field at a time with an explicit phase,
failure contract, and VM gate. Native kubeadm passthrough remains available as
desired input, but passthrough does not imply live-operation support.

## Plan And Command Surface

Planning is read-only. The explicit plan reports:

```text
config name and desired generation
desired and live canonical digests
supported field-level delta
Kubernetes payload version and digest
three-node order and coordinator
etcd member and snapshot preconditions
static manifest digests before mutation
commands that would run
unsupported differences and required manual action
```

A no-change plan exits distinctly from action-required, unsupported/manual,
and collection failure. Planning never writes `/etc/kubernetes`, calls a
mutating kubeadm phase, changes a ConfigMap, restarts kubelet, or changes a
generation.

Execution uses the kubeadm binary from the active Kubernetes sysext. Target
kubeadm private-mount access is unnecessary because the Kubernetes version is
unchanged.

On each node, the node-local mutating phase is:

```text
kubeadm init phase control-plane all \
  --config /etc/katl/kubeadm/<name>/config.yaml
```

After every node succeeds, the coordinator runs once:

```text
kubeadm init phase upload-config kubeadm \
  --config /etc/katl/kubeadm/<name>/config.yaml
```

Dry-run or rendered-output inspection must occur before the first mutation and
must prove that only the three control-plane manifests can change. Katl does not
run the `etcd local`, certificate, kubeconfig, kubelet, addon, bootstrap-token,
or upgrade phases for this operation.

## Three-Control-Plane Ordering

`katlctl` is the rollout coordinator; each node's `katlc` remains the authority
for its local mutation and `OperationRecord`. The explicit inventory identifies
one coordinator. The coordinator node is changed last:

```text
1. acquire the rollout plan and verify all cluster-wide preconditions
2. create and verify the required etcd snapshot evidence
3. mutate the first non-coordinator control plane
4. wait for its three static Pods, local API, node Ready state, stable API VIP,
   and etcd health
5. mutate the second non-coordinator and repeat every health check
6. mutate the coordinator and repeat every health check
7. upload the shared kubeadm ClusterConfiguration from the coordinator
8. verify the live ConfigMap digest, all manifest digests, three Ready nodes,
   stable API VIP, and three healthy etcd members
```

Only one node may own the mutating phase at a time. The coordinator stops at the
first refusal, timeout, failed command, or failed health check. It never moves
to the next node after an ambiguous result.

Katl cordons a node before its manifest mutation and restores its original
schedulability only after local and cluster health pass. The v0.1 operation does
not drain: the node is not rebooted, kubelet is not restarted, and evicting
ordinary workloads would add disruption unrelated to the three static Pods.

Regenerating a node's three manifests causes kubelet to restart those static
Pods. The rollout must observe new container identities and `profiling=false`
arguments for all three components before declaring that node complete.

## Etcd And Static Pod Safety

The operation never regenerates `etcd.yaml`, changes `/var/lib/etcd`, changes
membership, or runs an etcd mutation. Etcd health and snapshot evidence are
preconditions because the operation changes API-serving static Pods and later
updates a ConfigMap stored in etcd.

The snapshot is recovery evidence, not permission for automatic restore. Katl
does not restore it, remove a member, or claim etcd rollback after a failed
control-plane configuration operation.

Static manifest backups and SHA-256 values are retained in the restricted
operation directory before mutation. They are diagnostic and explicit-repair
inputs. The normal host-generation rollback path must not copy them back or
claim that live kubeadm state was restored.

## Online, Generation, And Rollback Semantics

This is an online live-state operation. It does not create or select another
generation and does not require a reboot. The desired configuration must
already be selected by the active committed generation.

Normal `katlc apply` may render and select a different desired kubeadm config,
but it only records `action-required`. It must not invoke kubeadm, kubectl,
crictl, restart kubelet, write `/etc/kubernetes`, or mutate Kubernetes objects.

Rolling a Katl generation backward changes desired input only. It does not
reverse static manifests or the kubeadm ConfigMap. Reversing a completed live
change requires a new explicit `kubeadm-control-plane-config` rollout whose
desired and expected-live digests describe that reverse transition. There is no
automatic post-mutation rollback in v0.1.

Failure before the first pre-exec mutation marker abandons the attempt without
claiming live change. Failure after any manifest write or ConfigMap upload stops
the rollout and sets `recoveryRequired`; host rollback is not Kubernetes repair.
The status names the last proven healthy node, the uncertain or failed node,
the manifest and ConfigMap digests observed, and the exact retry or reverse
operation required.

## Operation Records

Each node writes an `OperationRecord` with:

```text
operationKind: kubeadm-control-plane-config
scope and resource lock: kubeadm-state
rollout ID, node position, node count, and coordinator identity
actor, expected machine ID, cluster identity, and stable endpoint
desired generation ID and selected KubeadmConfig name
desired config path and canonical SHA-256
expected and observed live kubeadm ConfigMap SHA-256
active Kubernetes payload version and digest
supported field-level delta
snapshot reference, digest, revision, member-list digest, source etcd version,
  creation time, storage location, and operator identity
original node schedulability
before/after SHA-256 for all three control-plane manifests
before/after static Pod container identities and component health
API VIP, node Ready, and etcd member/endpoint health evidence
pre-exec mutation markers and redacted kubeadm invocations
whether the coordinator ConfigMap upload ran
terminal result, recoveryRequired, and next action
```

The node phase plan is:

```text
accepted
preflight-complete
cordon-complete
manifest-backup-complete
control-plane-manifests-running
control-plane-manifests-complete
post-manifest-health-complete
uncordon-complete
operation-complete
```

The coordinator record additionally contains:

```text
rollout-members-verified
kubeadm-config-upload-running
kubeadm-config-upload-complete
post-upload-health-complete
```

`katlctl` retains a non-authoritative rollout summary that references every
node-local operation ID and request digest. Node-local journals remain the
mutation authority.

## Consequences

The first release proves a meaningful live kubeadm-owned change while keeping
the safety surface small enough to validate exhaustively. Operators cannot use
this path as an arbitrary component-flag editor, but unsupported desired input
is reported precisely instead of being silently applied.

The coordinator and node-local executor are separate responsibilities. A
client interruption cannot make `katlc` forget a local mutation, and a node
failure cannot cause the coordinator to continue to another control plane.

Adding future fields requires deciding whether existing kubeadm phases remain
safe, whether certificates or etcd are involved, what health proof is needed,
and how a reverse operation works.

## Rejected Alternatives

Applying every `ClusterConfiguration` difference was rejected because kubeadm
fields have different ownership, restart, certificate, etcd, and rollback
semantics.

Treating `/etc/kubernetes/manifests` as confext output was rejected because the
files are persistent kubeadm-owned live state and must remain writable by
kubeadm.

Running the change during normal generation activation was rejected because a
boot or desired-input rollback must not hide a cluster mutation.

Regenerating the etcd manifest with the control-plane manifests was rejected
because this operation neither changes etcd configuration nor owns etcd
recovery.

Parallel control-plane mutation was rejected because it can remove API
capacity, disrupt controller leadership, and make failure ownership ambiguous.

Automatic manifest or snapshot rollback was rejected because an interrupted
static-Pod rewrite is external mutation and restoring a snapshot is a distinct
cluster-wide recovery operation.
