# Apply KatlOS Node Configuration

Use config apply for supported host configuration after installation. It is not
a general Kubernetes, disk, kubeadm, or operating-system upgrade mechanism.

## Supported Input

The normal source is the same `ClusterConfig` used for installation. The current
renderer carries:

- SSH authorized keys;
- systemd-networkd files; and
- operation-only system role and internally selected kubeadm profile state.

Runtime-safe fields can apply normally. Operation-only differences are planned
and reported as requiring an explicit lifecycle action; config apply does not
run kubeadm. Disk/install selection and Kubernetes version changes use the
dedicated install and Kubernetes upgrade workflows.

## Plan an Apply

An optional plan compiles the selected node configuration and asks the node to
validate it without accepting an operation:

```sh
katlctl config apply ./cluster.yaml \
  --node cp-1 \
  --plan
```

Validation reports the changed domains and accepted apply mode without
accepting an operation. The default `auto` lets the domain policy select live or
next-boot application. Request `--mode live` or `--mode next-boot` only when you
intend to constrain that policy; unsafe requests are refused.

If the source has already been compiled, use the expert bundle input instead of
the positional source:

```sh
katlctl config apply --config-bundle ./katl-lab.katlcfg --node cp-1 --plan
```

Katl derives and verifies the bundle's integrity metadata from the file.

## Apply the Reviewed Request

Run the same arguments without `--plan`:

```sh
katlctl config apply ./cluster.yaml --node cp-1
```

`katlctl` resolves the selected workstation context, reads the node credential,
derives the monotonically increasing desired version and candidate generation,
validates the change, follows the durable apply, and exits only after a terminal
result. Require `terminal: true` and `result: succeeded`. If `recoveryRequired`
is true, stop and follow `failureReason` and `nextAction`.

## Check Generation Status

Query the candidate through the agent:

```sh
katlctl config apply status \
  --node cp-1
```

The command selects the node's current generation automatically. Pass
`--generation` only to inspect an older or staged generation. For a live change,
require committed state and healthy config-apply evidence.
For a next-boot change, require committed staged state, reboot in a controlled
window, then require the candidate to become healthy after
`katl-boot-complete.target`.

On-node evidence remains available under:

```text
/var/lib/katl/generations/<generation>/
/var/lib/katl/operations/<operation-id>/
/var/lib/katl/boot/selection.json
```

If status reports rollback failure, `failed-needs-repair`, or a kubeadm action
requirement, stop and classify it before submitting another configuration.
