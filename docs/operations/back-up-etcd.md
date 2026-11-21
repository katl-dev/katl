# Back up etcd

Use this procedure to keep copies of the cluster's etcd data away from the
nodes. Katl takes etcd snapshots only during `katlctl kubernetes upgrade` and
does not schedule backups, copy snapshots off a node, prune them, or restore
them, so regular etcd backups are yours to run and store.

An etcd snapshot contains every Kubernetes API object, including Secrets
unless your platform encrypts them at rest. Protect each copy as you protect the
kubeconfig and the [Kubernetes identity](kubernetes-identity.md).

## Before you start

- The cluster was bootstrapped by Katl with stacked etcd on its control planes.
- You can log in to a control plane as `root` over SSH; see
  [Access installed nodes](access.md).

KatlOS does not install `etcdctl` or `etcdutl` on the host. The procedure runs
them inside the running etcd static-pod container with `crictl exec`, as Katl
does. The container shares `/var/lib/etcd` with the host, so the snapshot must
be written under that directory to be readable from the host.

## Save a snapshot

Take the snapshot on one healthy control plane. The snapshot covers the whole
cluster, so you do not need one from every control plane.

1. Log in to the control plane:

   ```sh
   ssh root@192.0.2.11
   ```

2. Find the etcd container and create the snapshot directory:

   ```sh
   id=$(crictl ps --name etcd --state Running -q)
   install -d -m 0700 /var/lib/etcd/katl-snapshots
   ```

   `crictl ps` must print exactly one container ID. If it prints none, etcd is
   not running on this node; use another control plane.

3. Save the snapshot. The `manual-` prefix keeps your files apart from the
   snapshots that Kubernetes upgrades write to the same directory:

   ```sh
   snapshot=/var/lib/etcd/katl-snapshots/manual-$(date -u +%Y%m%dT%H%M%SZ).db
   crictl exec "$id" etcdctl \
     --endpoints=https://127.0.0.1:2379 \
     --cacert=/etc/kubernetes/pki/etcd/ca.crt \
     --cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt \
     --key=/etc/kubernetes/pki/etcd/healthcheck-client.key \
     snapshot save "$snapshot"
   chmod 0600 "$snapshot"
   echo "$snapshot"
   ```

4. Check that the snapshot is readable and record its revision and size:

   ```sh
   crictl exec "$id" etcdutl snapshot status "$snapshot" -w table
   sha256sum "$snapshot"
   ```

## Copy the snapshot off the node

From your workstation, copy the file printed in step 3 and compare its
digest with the one recorded on the node:

```sh
scp root@192.0.2.11:/var/lib/etcd/katl-snapshots/manual-20261121T120000Z.db .
sha256sum manual-20261121T120000Z.db
```

Store the copy somewhere that survives the loss of every node, such as your
existing backup system. Then delete the node-local file, or keep a small
number of recent ones; nothing on the node removes `manual-` files for you.

## Pre-upgrade snapshots

Before the first mutating step of a Kubernetes upgrade, Katl saves a verified
snapshot on each pending control plane at
`/var/lib/etcd/katl-snapshots/OPERATION_ID.db`; see
[Execute the rollout](upgrade-kubernetes.md#execute-the-rollout). The upgrade
report records each snapshot's path, revision, and SHA-256 digest: the
`--output json` report includes them, and the text output prints them when the
rollout stops with recovery required. These files
are kept on the node's writable state storage, or on the dedicated etcd
partition when one was installed, and each upgrade adds another file about the
size of the etcd database. Katl never deletes them.

Delete a pre-upgrade snapshot only after the rollout that created it has
completed and you have checked the cluster. While its upgrade operation is
still active or needs recovery, Katl re-verifies the file's digest, and the
snapshot is the recovery point for that upgrade. Copy it off the node first if
you want to keep it.

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
