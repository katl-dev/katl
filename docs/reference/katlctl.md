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
| `katlctl management` | Inspect the automatic management-identity backup path or restore it on a workstation. |
| `katlctl system-extension` | Inspect, validate, publish, and query operator-owned system extensions. |

## Input Conventions

The normal cluster input is always `--config ./cluster.yaml`. Commands that
accept config also accept a compiled `.katlcfg` bundle through the same flag.
An optional saved `--context` can shorten repeated day-two commands, but it is
not a second desired-state source.

`--endpoint` overrides only the address used to contact one selected installer
or node. It does not change node identity, bypass mTLS, or grant access without
the matching saved cluster identity.

## Automatic management identity

`katlctl config init`, `config bundle`, and `install apply` automatically create
or reuse management trust keyed by the `ClusterConfig` name. There are no
routine TLS flags. Back up the `.katlkey` path printed at first creation; it
contains the authority needed to issue the same node identities during a
reinstall.

Recovery-only commands are:

```sh
katlctl management identity path homelab
katlctl management identity inspect ./homelab.katlkey
katlctl management identity import ./homelab.katlkey
```

Import is idempotent for the same identity and refuses to replace a different
identity for that cluster. The saved context contains only the operator client
leaf and never exposes it through `katlctl context show`.

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

Planning and dry-run flags do not authorize mutation. Destructive wipe and
storage acknowledgements are operation-specific; they are never persisted as
blanket consent in `ClusterConfig`.

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
