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

`katlctl config path` prints the resolved path.

## Initial Scope

The file starts as a minimal client-side profile store. It may grow to include:

```text
currentContext
contexts[]
clusters[].nodes[].managementEndpoint
clusters[].nodes[].systemRole
clusters[].nodes[].credentialRef
clusters[].controlPlaneEndpoint
```

The config must not contain inline bearer tokens, private keys, kubeconfigs, or
cluster PKI. Store references to credentials, not credential material.

Day-one commands may still accept explicit inventory or plan files. As day-two
operations such as Kubernetes upgrades are added, `katlctl` can use this config
to remember node management endpoints and roles between invocations.

## Boundary

`katlctl` config may help locate node-local `katlc` endpoints and select an
operator workflow target. Node-local `katlc` remains the only writer of
generation specs, generation status, boot selection, operation records, and
durable node lifecycle state.
