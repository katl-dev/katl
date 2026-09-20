# `katlctl` Command Map

`katlctl` is the public workstation client for KatlOS installation and bounded
lifecycle operations. Use the binary from the same release as the KatlOS
artifacts. Run `katlctl COMMAND --help` for the exact flags and examples in your
installed release.

| Command group | Purpose |
| --- | --- |
| `katlctl config` | Create, validate, inspect, compare, and compile `ClusterConfig`. |
| `katlctl install` | Discover a waiting installer, enable installer SSH, submit config, and inspect installation. |
| `katlctl cluster` | Inspect or apply the complete cluster, bootstrap kubeadm, inspect etcd, and perform explicit cluster wipes. |
| `katlctl node` | Inspect, reboot, shut down, upgrade, or explicitly wipe one node. |
| `katlctl kubernetes` | Create, import, and inspect reusable Kubernetes identity; plan and execute supported Kubernetes upgrades. |
| `katlctl operations` | Inspect current and recent durable node operations. |
| `katlctl context` | Save and select optional workstation topology shortcuts. |
| `katlctl management` | Create, export, inspect, and recover durable cluster management secrets. |
| `katlctl system-extension` | Inspect, validate, publish, and query operator-owned system extensions. |

## Input Conventions

The normal cluster input is always `--config ./cluster.yaml`. Commands that
accept config also accept a compiled `.katlcfg` bundle through the same flag.
An optional saved `--context` can shorten repeated day-two commands, but it is
not a second desired-state source.

`--endpoint` overrides only the address used to contact one selected installer
or node. It does not change node identity, bypass mTLS, or grant access without
the matching saved cluster identity.

## Cluster management secrets

`katlctl config init` creates a secrets file beside the configuration and
references it in `spec.managementIdentity`. Hand-written new cluster configs
use `management identity create --config CONFIG`; existing clusters use
`management identity export --config CONFIG` to preserve their original trust.
The file supports SOPS encryption. Routine bundle and install commands read it
and never generate a missing authority. There are no routine TLS flags.

Legacy workstation-store recovery and inspection commands are:

```sh
katlctl management identity path homelab
katlctl management identity inspect ./homelab.katlkey
katlctl management identity import ./homelab.katlkey
```

Import is idempotent for the same identity and refuses to replace a different
identity for that cluster. The saved context contains only the operator client
leaf and never exposes it through `katlctl context show`.

`config init` creates a configuration-referenced `management-secrets.yaml`;
this may be SOPS encrypted. Existing clusters can migrate their original
authority with `katlctl management identity export --config ./cluster.yaml`.
Keep that file across reinstalls. Workstation context is a disposable shortcut.

A reinstall under the same authority is accepted after TLS and node-name
verification. Commands observe the current installation and reject a change
during an operation. `context save --config ./cluster.yaml` refreshes saved
bindings automatically; no replacement flag is required.

Text output is designed for interactive use. Commands that expose `--output
json` provide the bounded automation surface. Progress is written separately
from the final result so scripts should consume the structured output rather
than parse human progress lines.

## Operation Semantics

Mutating commands submit idempotent, durable node operations and normally wait
for a terminal result. A lost workstation connection does not cancel accepted
node-local work. Use `--no-wait` only when intentionally detaching, then inspect
the result with:

```sh
katlctl operations list --config ./cluster.yaml --node cp-1
katlctl operations status --config ./cluster.yaml --node cp-1 --watch
```

Supply an operation ID only when selecting a particular historical record.
Use `--diagnostics verbose` when the normal redacted status does not contain
enough recovery evidence.

Planning and dry-run flags do not authorize mutation. For node volumes,
`wipe: true` authorizes erasing existing contents during provisioning, without
an additional acknowledgement. Replacing a generation-bound volume still
requires the operation-specific `--rebind-volume NODE/VOLUME` flag.

## Kubernetes identity

```sh
katlctl kubernetes identity create \
  --cluster-name homelab --output ./homelab-kubernetes-identity.katlkey
katlctl kubernetes identity inspect ./homelab-kubernetes-identity.katlkey
katlctl cluster bootstrap --config ./cluster.yaml \
  --identity ./homelab-kubernetes-identity.katlkey --init-node cp-1
```

The identity is a mode-`0600` operator secret, not a config field or publishable
install artifact. See [Preserve Kubernetes
identity](../operations/kubernetes-identity.md) for import, rebuild, security,
and backup semantics.
