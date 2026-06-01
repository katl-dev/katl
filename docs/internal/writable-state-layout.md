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

## Required Directories

`katlos-install` or first-boot tmpfiles rules must ensure these directories
exist on the state partition:

| Directory | Owner | Mode | Purpose |
| --- | --- | --- | --- |
| `/var/lib/katl` | `root:root` | `0755` | Katl persistent state root |
| `/var/lib/katl/generations` | `root:root` | `0755` | Per-generation records, staged extension content, and boot status |
| `/var/lib/katl/generations/<id>` | `root:root` | `0755` | One root/sysext/confext generation |
| `/var/lib/katl/generations/<id>/metadata.json` | `root:root` | `0644` | Generation selection plus mutable boot/health status fields |
| `/var/lib/katl/generations/<id>/confext` | `root:root` | `0755` | Generated confext tree or image for the generation |
| `/var/lib/katl/generations/<id>/sysext` | `root:root` | `0755` | Sysext artifacts selected with the generation |
| `/var/lib/katl/identity` | `root:root` | `0755` | Stable machine identity backing files |
| `/var/lib/katl/identity/machine-id` | `root:root` | `0444` | Stable systemd machine ID backing file |
| `/var/lib/katl/kubernetes` | `root:root` | `0755` | Kubernetes projected state namespace |
| `/var/lib/katl/kubernetes/etc-kubernetes` | `root:root` | `0755` | Backing store for projected `/etc/kubernetes` |
| `/var/lib/katl/ssh` | `root:root` | `0755` | SSH projected state namespace |
| `/var/lib/katl/ssh/host-keys` | `root:root` | `0700` | Backing store for persistent SSH host keys |
| `/var/lib/kubelet` | `root:root` | created by package/tmpfiles | Kubelet native persistent state |
| `/var/lib/containerd` | `root:root` | created by package/tmpfiles | Containerd native persistent state |
| `/var/lib/etcd` | `root:root` | created by kubeadm/etcd or mount | Etcd data when not using a dedicated etcd partition |
| `/var/log/journal` | `root:systemd-journal` | created by systemd-journald | Persistent journal, only when enabled |

Generation content is immutable after creation except through explicit repair
tooling. In the first metadata schema, `metadata.json` also carries mutable
`bootState` and `healthState` fields. Those status fields may be updated by boot
health, rollback, or repair tooling; root slot, UKI, sysext, and confext
selection fields must not be changed in place. Mutable pointers such as
"current" should not live inside an individual generation directory.

## Activation State

At boot, Katl may create these ephemeral paths from generation metadata:

```text
/run/extensions/<selected sysext>
/run/confexts/<selected confext>
```

These are not persistent state. They must be recreated every boot after `/var`
is mounted and before `systemd-sysext.service` or `systemd-confext.service`
runs.

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

## Follow-up Gates

Mount units and tmpfiles snippets should be verified with `systemd-analyze
verify` once they exist. QEMU boot tests must prove that `/var` is mounted by
stable partition identity and that no persistent identity is read from `/run`.
