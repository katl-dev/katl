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
| `katlctl kubernetes` | Plan and execute supported Kubernetes upgrades. |
| `katlctl operations` | Inspect current and recent durable node operations. |
| `katlctl context` | Save and select optional workstation topology shortcuts. |
| `katlctl system-extension` | Inspect, validate, publish, and query operator-owned system extensions. |

## Input Conventions

The normal cluster input is always `--config ./cluster.yaml`. Commands that
accept config also accept a compiled `.katlcfg` bundle through the same flag.
An optional saved `--context` can shorten repeated day-two commands, but it is
not a second desired-state source.

`--endpoint` overrides only the address used to contact one selected installer
or node. It does not change node identity or retained configuration.

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
