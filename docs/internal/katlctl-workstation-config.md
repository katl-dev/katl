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

The file is a minimal client-side profile store. `katlctl cluster enroll
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
    credentialRef: file:/secure/katl/cp-1.token
  - name: worker-1
    managementEndpoint: worker-1.prod.example:9443
    systemRole: worker
    credentialRef: file:/secure/katl/worker-1.token
```

`currentContext` names a context in `contexts`. Each context names a cluster in
`clusters`. Each cluster records node-local `katlc` management endpoints,
KatlOS system roles, credential references, and optionally the stable
control-plane endpoint used by operator workflows.

The config must not contain inline bearer tokens, private keys, kubeconfigs, or
cluster PKI. Enrollment stores per-node bearer tokens beneath the adjacent
`credentials/<cluster>/` directory with mode `0600` and records only `file:`
references here.

`katlctl context show` prints the resolved context topology as JSON.
The topology output includes `credentialRef` values because they are operator
references needed by orchestration, not credential material.

## Precedence

Explicit operator input is authoritative. A compiled plan, when accepted by a
command, wins over explicit inventory. Explicit inventory wins over workstation
config. Workstation `currentContext` is used only when a command asks for a
profile and no explicit inventory or plan input supplies the same topology.

Invocation flags that are already explicit command overrides, such as
`--control-plane-endpoint`, `--init-node`, or `--node-address`, remain command
overrides after the topology source is selected. `katlctl` must not silently
borrow missing endpoints, roles, or credentials from workstation config when an
explicit inventory or plan is present.

## Boundary

`katlctl` config may help locate node-local `katlc` endpoints and select an
operator workflow target. Node-local `katlc` remains the only writer of
generation specs, generation status, boot selection, operation records, and
durable node lifecycle state.
