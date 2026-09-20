# `katlctl` Command Map

`katlctl` is the public workstation client for KatlOS installation and bounded
lifecycle operations. Use the binary from the same release as the KatlOS
artifacts. Run `katlctl COMMAND --help` for the exact flags and examples in your
installed release.

| Command group | Purpose |
| --- | --- |
| `katlctl config` | Create, validate, inspect, compare, and compile `ClusterConfig`. |
| `katlctl install` | Discover a waiting installer, enable installer SSH, submit config, and inspect installation. |
| `katlctl cluster` | Inspect or apply selected nodes, bootstrap kubeadm, retrieve kubeconfig, inspect etcd, and perform explicit cluster wipes. |
| `katlctl node` | Inspect status and logs, join, reboot, shut down, upgrade, or explicitly wipe one node. |
| `katlctl kubernetes` | Create, import, and inspect reusable Kubernetes identity; plan and execute supported Kubernetes upgrades. |
| `katlctl operations` | Inspect current and recent durable node operations. |
| `katlctl context` | Save, select, inspect, and delete optional workstation topology shortcuts. |
| `katlctl management` | Create, export, inspect, and recover durable cluster management secrets. |
| `katlctl system-extension` | Inspect, validate, publish, and query operator-owned system extensions. |

## Input Conventions

The normal cluster input is always `--config ./cluster.yaml`. Commands that
accept config also accept a compiled `.katlcfg` bundle through the same flag.
An optional saved `--context` can shorten repeated day-two commands, but it is
not a second desired-state source.

`--endpoint` overrides only the address used to contact one selected installer
or node. It does not change node identity or bypass the selected authentication
mode. An address-only connection without a selected context uses trusted-network
mode and cannot access an mTLS node.

## Cluster management secrets

New configurations default to `managementAuthentication: trusted-network` and
need no management keys. The following credential commands apply to opt-in
`managementAuthentication: mtls`. See [management access](../operations/access.md)
for mode selection and existing-installation behavior.

`katlctl config init --management-authentication mtls` creates a secrets file beside the configuration and
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

`config init --management-authentication mtls` creates a configuration-referenced `management-secrets.yaml`;
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

## Operator journey

Prefix each command below with `katlctl`.

| Task | Command |
| --- | --- |
| Prepare and check configuration | `config init ./cluster.yaml`, then `config validate ./cluster.yaml` |
| Find PXE-booted installers | `install discover` |
| Install each node | `install apply --config ./cluster.yaml --node cp-1` |
| Inspect an installer | `install status cp-1 --config ./cluster.yaml` |
| Start Kubernetes | `cluster bootstrap --config ./cluster.yaml --init-node cp-1` |
| Join another installed node | `node join worker-1 --config ./cluster.yaml` |
| Preview configuration changes | `cluster apply --config ./cluster.yaml --node cp-1 --plan` |
| Apply configuration | Repeat without `--plan` |
| Upgrade KatlOS | `node upgrade cp-1 --config ./cluster.yaml --version VERSION --plan`, then repeat without `--plan` |
| Switch kernel flavour | `node upgrade cp-1 --config ./cluster.yaml --version VERSION --flavour lts` (or `standard`) |
| Upgrade Kubernetes | Change the version in config, then `kubernetes upgrade --config ./cluster.yaml --plan` |
| Get Kubernetes access on another workstation | `cluster kubeconfig ./kubeconfig --config ./cluster.yaml` |
| Inspect health | `cluster status --config ./cluster.yaml` |
| Diagnose a node | `node logs cp-1 --config ./cluster.yaml -u kubelet` |
| Reprovision | `node wipe cp-1 --config ./cluster.yaml --plan`, then follow the reviewed wipe/reinstall journey |

Install commands talk to the installer on TCP 8080. Once the node boots its
installed system, use node/cluster commands on TCP 9443. `install apply`
replaces the system disk; it is not a day-two apply. Bootstrap prepares
Kubernetes. Install your CNI and GitOps/workloads separately, or supply bootstrap
`--manifest`, `--pre-wait`, and `--wait` inputs.

Commands using `--config` observe the current installation in memory. They do
not create or change a saved context. Only explicit context commands persist
that shortcut. `context delete NAME` removes it without changing any node or
external secrets. Deleting the current selection never selects another cluster.

Installed-node commands accept `NODE` or `--node NAME`, not both. Selection is
optional when the source has one node. `cluster apply --node NAME` is repeatable
and defaults to all configured nodes. Bootstrap's `--init-node` selects the
initial control plane rather than limiting cluster membership.

Use `--plan` for bootstrap, apply, upgrade, and wipe previews. Bootstrap retains
`--dry-run` as an alias. Apply plans validate host configuration on selected
nodes without accepting operations; Kubernetes component readiness is checked
during execution. A plan does not reserve the observed state.

`cluster apply --mode auto` applies safe live changes and stages changes needing
a reboot. `--mode live` refuses reboot-requiring changes; `--mode next-boot`
stages host changes. Reboot staged nodes, then repeat apply to finish deferred
Kubernetes configuration. Apply never implicitly joins nodes or upgrades
Kubernetes. The hidden rendered-node interface is for development fixtures and
does not accept ClusterConfig or bundles.

Timeout flags use Go durations, such as `90s` or `15m`, and must be positive.
Bootstrap and apply default to a 30-minute overall deadline; wipe is capped at
25 minutes. Other commands describe their deadline or wait scope in `--help`.
Ctrl+C stops the workstation wait. Accepted node operations remain durable:
inspect `operations status --watch` and resume the original command where
supported instead of assuming nothing happened or immediately wiping the node.

## Diagnostics and access

```sh
katlctl node logs cp-1 --config ./cluster.yaml --unit kubelet --follow
katlctl node logs cp-1 --config ./cluster.yaml --boot=-1 --lines 200
katlctl node logs cp-1 --config ./cluster.yaml --since '1 hour ago' -o json
katlctl cluster kubeconfig ./kubeconfig --config ./cluster.yaml
```

`node logs --output json` streams newline-delimited native journal objects.
Other JSON commands emit a final document. Progress goes to stderr. File-producing
commands use `--output` for a path where their help says so, such as `config bundle`.

Kubeconfig retrieval tries configured control planes, or uses `--node` when
specified. It defaults to the configured API endpoint; `--server` and optionally
`--tls-server-name` allow an alternate route. It does not assert that the endpoint
is reachable. Files are mode 0600; different existing content requires `--force`.
It neither merges into `~/.kube/config` nor switches a kubectl context. Command
output reports the path and endpoint, never credential contents.

The management API must be reachable for logs and kubeconfig retrieval. Use SSH
or the physical/serial console for failed boot, network, or agent recovery.
`kubectl`, your CNI installer, and GitOps remain the interfaces for Kubernetes
workloads. Katlctl does not provide an arbitrary remote shell, an automatic fleet
rollout scheduler, or a complete etcd backup/restore system.

Generate shell completion with `katlctl completion bash`, `fish`, `zsh`, or
`powershell`, and install it using your shell's completion convention.

Bootstrap reads management and Kubernetes addresses from ClusterConfig. Set
`management.address` and, when distinct, `kubernetes.address` there so every
readiness and bootstrap phase uses consistent targets. The legacy `--node-address`
override is restricted to advanced inventory input. The older `node upgrade
VERSION NODE` invocation remains accepted; new scripts should use the explicit
`--version` form shown above.
