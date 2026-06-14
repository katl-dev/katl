# Writable State Partition Layout

This decision defines the first filesystem layout for the Katl writable state
partition mounted at `/var`.

## Decision

Katl creates one root-disk state partition with GPT label `KATL_STATE`, type
`var`, and filesystem `ext4` for the first implementation. The installer and
boot metadata should prefer stable partition identity in this order:

```text
PARTUUID recorded from the installed state partition
GPT label KATL_STATE as a local validation hint
systemd-gpt-auto type var only when the target disk is unambiguous
```

Persistent identity must not be stored in `/run`. `/run` is only for boot-local
activation links and service handoff state that can be regenerated from `/var`.

## Etcd Data Placement

The first supported path keeps etcd data at `/var/lib/etcd` on the `KATL_STATE`
partition mounted at `/var`. Kubeadm or the etcd static pod owns the directory
contents. Katl only guarantees that `/var` is mounted before kubeadm-managed
control-plane services need the path. This keeps worker nodes, single-node
experiments, and control-plane nodes on the same base install layout until the
installer has role-aware storage policy.

A dedicated etcd data partition is a future Katl-owned root-disk partition, not
an `extraDisks` mount. When exposed in the install manifest, it should be a
first-class field under install storage, for example:

```json
{
  "install": {
    "storage": {
      "etcd": {
        "dedicatedPartition": {
          "sizeMiB": 16384,
          "filesystem": "ext4"
        }
      }
    }
  }
}
```

That request means: carve a `KATL_ETCD` partition from the selected target root
disk, format it with the requested filesystem, and mount it at `/var/lib/etcd`.
It must reduce the remaining `KATL_STATE` size and fail planning if the target
disk cannot still satisfy both root slots and the minimum state partition size.
The initial filesystem should be `ext4`; adding `xfs` support needs explicit
validation and mount-unit coverage.

Unsafe cases must be rejected rather than interpreted as etcd storage:

- `install.extraDisks[].mount.path` equal to `/var/lib/etcd` or any parent or
  child path that would shadow it.
- Extra-disk selectors that resolve to the selected target root disk or one of
  its partitions.
- Dedicated etcd partition requests without a positive size, with a filesystem
  outside the supported allowlist, or that leave less than the minimum state
  partition size.
- Any attempt to store etcd under `/run`, `/etc`, `/usr`, `/tmp`, kubelet state,
  containerd state, or Katl generation state.

## Mount And Service Ordering Rules

Katl should generate the smallest mount surface that preserves normal Linux
paths for services:

| Unit | What | Where | Rule |
| --- | --- | --- | --- |
| `var.mount` | installed `KATL_STATE` partition by PARTUUID | `/var` | Required local filesystem; no `nofail`; must be active before any persistent node service starts |
| `etc-kubernetes.mount` | `/var/lib/katl/kubernetes/etc-kubernetes` bind source | `/etc/kubernetes` | Only writable `/etc` projection in the first implementation; active after confext and before kubelet or kubeadm automation |
| `var-lib-etcd.mount` | optional installed `KATL_ETCD` partition by PARTUUID | `/var/lib/etcd` | Generated only when a dedicated etcd partition was planned; active before kubelet, kubeadm automation, and the kubeadm-ready target |

No mount units are generated for `/var/lib/kubelet` or
`/var/lib/containerd` in the default model. They are native directories on the
`KATL_STATE` filesystem mounted at `/var`. Katl-owned service drop-ins should
express that dependency with `RequiresMountsFor=` rather than bind mounting
those paths:

```text
containerd.service
  After=var.mount
  RequiresMountsFor=/var/lib/containerd

kubelet.service
  After=var.mount containerd.service etc-kubernetes.mount
  RequiresMountsFor=/var/lib/kubelet /etc/kubernetes

katl kubeadm init/join units, when present
  After=var.mount containerd.service etc-kubernetes.mount
  RequiresMountsFor=/etc/kubernetes /etc/katl

katl-kubeadm-ready.target
  After=containerd.service etc-kubernetes.mount
```

When `var-lib-etcd.mount` exists, kubelet, kubeadm automation units, and
`katl-kubeadm-ready.target` must also order after it and require
`/var/lib/etcd` with `RequiresMountsFor=`. When it does not exist,
`/var/lib/etcd` is just a native directory on `/var`, and the `/var` dependency
is sufficient.

The ordering rule is intentionally about local prerequisites only. Katl may
require containerd and projected Kubernetes state before declaring the runtime
kubeadm-ready, but kubeadm and user-managed cluster tooling still own control
plane convergence.

## Required Directories

`katlos-install` or first-boot tmpfiles rules must ensure these directories
exist on the state partition:

| Directory | Owner | Mode | Purpose |
| --- | --- | --- | --- |
| `/var/lib/katl` | `root:root` | `0755` | Katl persistent state root |
| `/var/lib/katl/boot` | `root:root` | `0755` | Durable boot-selection state |
| `/var/lib/katl/boot/selection.json` | `root:root` | `0644` | Current default, trial, previous known-good, and booted generation pointers |
| `/var/lib/katl/generations` | `root:root` | `0755` | Per-generation records, staged extension content, and boot status |
| `/var/lib/katl/generations/<id>` | `root:root` | `0755` | One root/sysext/confext generation |
| `/var/lib/katl/generations/<id>/spec.json` | `root:root` | `0644` | Immutable generation selection: root, UKI, sysext, confext, kernel command line, compatibility |
| `/var/lib/katl/generations/<id>/status.json` | `root:root` | `0644` | Mutable commit, boot, and health status bound to spec by `specDigest` |
| `/var/lib/katl/generations/<id>/confext` | `root:root` | `0755` | Generated confext tree or image for the generation |
| `/var/lib/katl/generations/<id>/sysext` | `root:root` | `0755` | Sysext artifacts selected with the generation |
| `/var/lib/katl/operations` | `root:root` | `0750` | Durable operation records, including deferred recovery and repair records |
| `/var/lib/katl/operations/<id>/record.json` | `root:root` | `0600` | Recoverable snapshot of request, plan, status, scope, lifecycle state, and redacted diagnostics |
| `/var/lib/katl/operations/<id>/journal` | `root:root` | `0700` | Append-only operation event journal used for crash recovery |
| `/var/lib/katl/operations/<id>/journal/<seq>.<event-id>.json` | `root:root` | `0600` | One durable operation event |
| `/var/lib/katl/operations/<id>/attachments` | `root:root` | `0700` | Redacted diagnostic artifacts referenced by the operation record |
| `/var/lib/katl/cluster` | `root:root` | `0750` | Katl-owned non-secret cluster intent namespace |
| `/var/lib/katl/cluster/intent.json` | `root:root` | `0600` | Normalized generation 0 cluster intent used by later bootstrap/join operations; not kubeadm output or cluster secret backup |
| `/var/lib/katl/config-requests` | `root:root` | `0750` | Request decision index for node configuration changes; accepted entries link to canonical operation IDs |
| `/var/lib/katl/identity` | `root:root` | `0755` | Stable machine identity backing files |
| `/var/lib/katl/identity/machine-id` | `root:root` | `0444` | Random install-generated systemd machine ID backing file |
| `/var/lib/katl/kubernetes` | `root:root` | `0755` | Kubernetes projected state namespace |
| `/var/lib/katl/kubernetes/etc-kubernetes` | `root:root` | `0755` | Backing store for projected `/etc/kubernetes` |
| `/var/lib/katl/ssh` | `root:root` | `0755` | SSH projected state namespace |
| `/var/lib/katl/ssh/host-keys` | `root:root` | `0700` | Backing store for persistent SSH host keys |
| `/var/lib/kubelet` | `root:root` | created by package/tmpfiles | Kubelet native persistent state |
| `/var/lib/containerd` | `root:root` | created by package/tmpfiles | Containerd native persistent state |
| `/var/lib/etcd` | `root:root` | created by kubeadm/etcd or mount | Etcd data when not using a dedicated etcd partition |
| `/var/log/journal` | `root:systemd-journal` | created by systemd-journald | Persistent journal, only when enabled |

Generation content and `spec.json` are immutable after creation. Only
`status.json` is mutable, and status updates must validate `specDigest` before
changing `commitState`, `bootState`, or `healthState`. Root slot, UKI, sysext,
and confext selection fields must not be changed in place. Mutable pointers such
as "current" should not live inside an individual generation directory.

Operation records may reference generation IDs, install checkpoints, or
Kubernetes artifacts, but they are not generation artifacts and must not be
activated through sysext/confext selection. Canonical operation records live
under `/var/lib/katl/operations/<id>/`. Existing workflow-specific files such as
install status, config apply status, bootstrap summaries, and upgrade summaries
may remain as compatibility read models, but they are not authoritative for
crash recovery unless they are backed by the `katlc`-owned operation journal.

`record.json` is a snapshot, not the only durable truth. Once an operation
becomes durable, journal entries under
`/var/lib/katl/operations/<id>/journal/` are authoritative for crash recovery.
If `record.json` is missing, older than the journal, or digest-invalid, Katl
rebuilds it from the highest valid journal sequence.

`katlos-install` creates `/var/lib/katl/identity/machine-id` with a random
machine ID during install. It is stable across normal boots and updates because
it lives on the state partition, but it is not deterministic and does not need
to survive a destructive reinstall. The backing file should be root-owned and
write-protected after install. `katlos-install` also renders that same value
into generation 0 boot metadata with `systemd.machine_id=<id>`. Future
generation writers must preserve the persistent value and reject loader entries
or kernel arguments that would introduce a different machine ID.

## Activation State

At boot, `katl-generation-activate.service` must recreate these ephemeral paths
from the selected generation spec:

```text
/run/extensions/<selected sysext>
/run/confexts/<selected confext>
/run/katl/locks/
/run/katl/operation-gates/
```

These are not persistent state. They must be recreated every boot after `/var`
is mounted and before `systemd-sysext.service` or `systemd-confext.service`
runs. Runtime locks under `/run/katl/locks` coordinate processes only; they are
not persistent identity or commit state. Operation gate tokens under
`/run/katl/operation-gates` are boot-local state regenerated from durable
operation records.

`/run/extensions` and `/run/confexts` are boot-local activation state, not
durable selection or commit state. Durable generation selection must come from
generation spec plus `/var/lib/katl/boot/selection.json`, not from a mutable
`current` symlink inside a generation directory.

Boot-selection state changes must follow the transaction model in
`docs/internal/boot-selection-transaction.md`. GPT labels, sysupdate filenames,
and `/run` links are validation or execution details, not the persistent default
or known-good source of truth.

The default activation mechanism is a service, not a systemd generator:

```text
katl-generation-activate.service
  Requires=var.mount
  After=var.mount
  Before=systemd-sysext.service systemd-confext.service
  RequiredBy=systemd-sysext.service systemd-confext.service
```

A generator cannot be the default because selected extension state lives under
`/var`, and a generator cannot order itself after `var.mount` or fail closed with
service dependency semantics. `katl-generation-activate.service` clears stale
`/run/extensions` and `/run/confexts`, recreates them as boot-local directories,
and links only the selected generation's extensions. If validation fails, old
generation links must not remain available; dependent sysext/confext activation
fails, boot health fails, and rollback selects the previous known-good
generation where available.

## Directories Left To Systemd Or Packages

Katl should not pre-create every application-owned subdirectory. These paths are
left to package defaults, tmpfiles, or the owning service unless a later task
finds an ordering problem:

```text
/var/cache
/var/lib/cni
/var/lib/containers
/var/lib/private
/var/log
/var/tmp
```

Kubelet and containerd package tmpfiles may create deeper subdirectories below
their state roots. Katl's responsibility is that `/var` is mounted and the
top-level persistent view is available before those services start.

The `/etc/kubernetes` projection from
`/var/lib/katl/kubernetes/etc-kubernetes` is defined in
`docs/internal/etc-kubernetes-projection.md`.

The stacked-etcd bootstrap and data ownership policy is defined in
`docs/internal/stacked-etcd-bootstrap-data-policy.md`.

Cluster-global bootstrap artifact ownership is defined in
`docs/internal/cluster-bootstrap-state-model.md`. General cluster rebuild means
destructive wipe/reinstall followed by a new bootstrap unless an explicit
same-cluster restore workflow is designed and tested.

## Follow-up Gates

Mount units and tmpfiles snippets should be verified with `systemd-analyze
verify` once they exist. VM boot tests must prove that `/var` is mounted by
stable partition identity and that no persistent identity is read from `/run`.
