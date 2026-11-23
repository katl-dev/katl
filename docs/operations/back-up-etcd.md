# Back up etcd

Use this procedure to keep copies of the cluster's etcd data away from the
nodes. `katlctl cluster etcd snapshot` saves a verified snapshot on your
workstation; Katl does not schedule backups or restore them, so running and
storing regular etcd backups is up to you.

An etcd snapshot contains every Kubernetes API object, including Secrets
unless your platform encrypts them at rest. Protect each copy as you protect the
kubeconfig and the [Kubernetes identity](kubernetes-identity.md).

## Before you start

- The cluster was bootstrapped by Katl with stacked etcd on its control planes.
- Your workstation can reach the control planes' management API with the same
  `--config` you use for other `katlctl cluster` commands.

## Save a snapshot

One snapshot covers the whole cluster, so you need it from only one control
plane. Name a new file for it:

```sh
katlctl cluster etcd snapshot --config ./cluster.yaml \
  --output etcd-20261121.db
```

Katl tries the control planes in name order, or only the one you name with
`--node`. The first one whose local etcd member is healthy, and whose cluster
has quorum, saves the snapshot with `etcdctl` inside its etcd container and
checks it with the file's embedded checksum and `etcdutl snapshot status`. The
file then streams to your workstation, where `katlctl` writes it with mode
`0600` only after its size and SHA-256 digest match what the node computed. On
success, the command prints the file path, the node, the etcd cluster and
member IDs, and the snapshot revision, key count, size, and SHA-256 digest.

A control plane that is unreachable, unhealthy, out of space for the
snapshot, or running a KatlOS release without this command is reported on
stderr and skipped. With `--node`, the command stops at that node instead.

The node keeps its copy only while the transfer runs. Katl never overwrites an
existing `--output` file, and a failed transfer leaves no partial file, so you
can rerun the same command. The node needs free space for one database copy
plus a 512 MiB reserve beside its etcd data, and the command refuses databases
larger than 8 GiB. `--timeout` (default 15 minutes) bounds the snapshot and
the transfer together.

Store the file somewhere that survives the loss of every node, such as your
existing backup system.

## Pre-upgrade snapshots

Before the first mutating step of a Kubernetes upgrade, Katl saves a verified
snapshot on each pending control plane at
`/var/lib/etcd/katl-snapshots/OPERATION_ID.db`; see
[Execute the rollout](upgrade-kubernetes.md#execute-the-rollout). The upgrade
report records each snapshot's path, revision, and SHA-256 digest: the
`--output json` report includes them, and the text output prints them when the
rollout stops with recovery required. These files
are kept on the node's writable state storage, and each is about the size of
the etcd database.

When a control plane's upgrade succeeds, Katl keeps the two newest pre-upgrade
snapshots on that node and deletes older ones. It never deletes a snapshot
whose upgrade operation is still active or needs recovery, because Katl
re-verifies that file's digest and it is the recovery point for that upgrade;
such a snapshot is kept in addition to the newest two. The upgrade report lists
the paths kept on each control plane: the text output prints a
`Retained etcd snapshots on NODE:` line, and the `--output json` report
includes them as `retained_snapshots` in each node's `etcd` evidence. Pruning
considers only files that Katl named after upgrade operations, so other files
in the directory, such as snapshots you saved there by hand, are left alone. Copy a
pre-upgrade snapshot off the node if you want to keep it beyond two later
upgrades.

## Restore limitations

Katl does not restore etcd snapshots, and this documentation does not describe
a KatlOS-specific restore procedure. A snapshot is input to upstream
[etcd disaster recovery](https://etcd.io/docs/v3.6/op-guide/recovery/), which
Katl neither runs nor verifies on KatlOS. Do not restore a snapshot over a
serving quorum.

The path Katl supports after losing quorum is reconstruction: follow
[whole-cluster wipe and reinstall](wipe-reinstall.md#plan-a-whole-cluster-wipe),
then [bootstrap Kubernetes](bootstrap-kubernetes.md) with your preserved
[Kubernetes identity](kubernetes-identity.md), reapply your GitOps entrypoint,
and restore workload data with your backup tool. This creates a new etcd
cluster rather than restoring the old Kubernetes API state.
