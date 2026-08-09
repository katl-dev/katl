# Katlctl Workstation Config

Status: current decision.

`katlctl` may keep local workstation configuration for communication profiles
and known node details. This state is operator convenience only. It is not
KatlOS node lifecycle state, desired cluster state, retry state, or recovery
state.

## Location

The default config file is:

```text
$XDG_CONFIG_HOME/katl/katlctl.yaml
```

When `XDG_CONFIG_HOME` is unset, use the platform user config directory. On
Linux this normally resolves to:

```text
$HOME/.config/katl/katlctl.yaml
```

Environment overrides are resolved in this order:

```text
KATLCTL_CONFIG
  full path to the katlctl config file

KATLCTL_CONFIG_DIR
  directory containing katlctl.yaml

XDG_CONFIG_HOME
  base config directory; katlctl appends katl/katlctl.yaml
```

`katlctl context path` prints the resolved path.

## Schema

The file is a minimal client-side profile store. `katlctl context save
SOURCE` creates or updates it on the normal path; operators do not need to
author this YAML by hand:

```yaml
currentContext: prod
contexts:
- name: prod
  cluster: katl-prod
clusters:
- name: katl-prod
  controlPlaneEndpoint: api.prod.example:6443
  nodes:
  - name: cp-1
    managementEndpoint: cp-1.prod.example:9443
    systemRole: control-plane
    enrollmentID: 0123456789abcdef0123456789abcdef
    machineID: fedcba9876543210fedcba9876543210
  - name: worker-1
    managementEndpoint: worker-1.prod.example:9443
    systemRole: worker
    enrollmentID: 11111111111111112222222222222222
    machineID: 33333333333333334444444444444444
```

`currentContext` names a context in `contexts`. Each context names a cluster in
`clusters`. Each cluster records node-local `katlc` management endpoints,
KatlOS system roles, the immutable install enrollment and machine identities,
and optionally the stable control-plane endpoint used by operator workflows.
The identities are public opaque values, not credentials.

`katlctl context show` prints the resolved context topology as JSON.

## Precedence

ClusterConfig and compiled plans remain authoritative for desired topology and
configuration. The workstation record is authoritative for the observed
enrollment binding and day-two management address. A mutation must join these
two sources by cluster and node name and refuse a missing or mismatched binding.

Bootstrap-only flags such as `--control-plane-endpoint` and `--init-node`
remain desired-state overrides. A mutation cannot override an enrolled
management address with `--endpoint`; the operator must use `katlctl context
rebind`, which verifies the saved enrollment and machine identities before
atomically changing the address.

## Boundary

`katlctl` config binds operator inventory identities to node-local `katlc`
endpoints. Node-local `katlc` remains the only writer of
generation specs, generation status, boot selection, operation records, and
durable node lifecycle state.
