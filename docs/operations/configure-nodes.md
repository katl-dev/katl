# Apply KatlOS Node Configuration

Use `katlctl node apply` for supported node configuration after installation. It is not
a general Kubernetes, disk, kubeadm, or operating-system upgrade mechanism.

## Supported Input

The normal source is the same `ClusterConfig` used for installation. The current
renderer carries:

- SSH authorized keys;
- systemd-networkd files; and
- operation-only system role and role-dependent Kubernetes bootstrap state.

Runtime-safe fields can apply normally. Operation-only differences are planned
and reported as requiring an explicit lifecycle action; node apply does not
run kubeadm. Disk/install selection and Kubernetes version changes use the
dedicated install and Kubernetes upgrade workflows.

If `spec.kubernetes.kubeadm` changes, planning reports a kubeadm-aware action.
Normal node apply does not rewrite live kubeadm or Kubernetes state; use the
dedicated operation for a supported change, or follow the reported manual
boundary when Katl does not support that transition.

## Plan an Apply

An optional plan compiles the selected node configuration and asks the node to
validate it without accepting an operation:

```sh
katlctl node apply cp-1 --config ./cluster.yaml \
	--plan
```

Validation reports the changed domains and accepted apply mode without
accepting an operation. The default `auto` lets the domain policy select live or
next-boot application. Request `--mode live` or `--mode next-boot` only when you
intend to constrain that policy; unsafe requests are refused.

If the source has already been compiled, pass the bundle through the same flag:

```sh
katlctl node apply cp-1 --config ./katl-lab.katlcfg --plan
```

Katl derives and verifies the bundle's integrity metadata from the file.

## Apply the Reviewed Request

Run the same arguments without `--plan`:

```sh
katlctl node apply cp-1 --config ./cluster.yaml
```

`katlctl` resolves the selected workstation context, reads the node credential,
derives the monotonically increasing desired version and candidate generation,
validates the change, follows the durable apply, and exits only after a terminal
result. Require `terminal: true` and `result: succeeded`. If `recoveryRequired`
is true, stop and follow `failureReason` and `nextAction`.

## Check Status

Use `katlctl node status cp-1 --config ./cluster.yaml` for the current healthy
generation. Use `katlctl operations list --config ./cluster.yaml --node cp-1`
when diagnosing an accepted or recently completed configuration operation.

On-node evidence remains available under:

```text
/var/lib/katl/generations/<generation>/
/var/lib/katl/operations/<operation-id>/
/var/lib/katl/boot/selection.json
```

If status reports rollback failure, `failed-needs-repair`, or a kubeadm action
requirement, stop and classify it before submitting another configuration.
