# Supported Node Configuration Domains

Status: current decision; amended by ADR-011.

ADR-011 adds bounded operator-authored native host configuration file sets.
Typed domains below remain the contract for Katl-owned product intent, but
operators may also provide validated regular files beneath `/etc` when they do
not collide with Katl-owned or protected paths. Such files use built-in live
adapters, bounded systemd notifications, or conservative next-boot fallback.
References below to arbitrary `/etc` patching and general systemd passthrough
continue to reject unbounded mutation, raw commands, service enablement, and
protected paths; they do not reject the ADR-011 file-set contract.

This document defines the initial Katl-owned node configuration domains for
install-time materialization and later runtime configuration changes. It narrows
the general generated-confext decision in
`docs/internal/adrs/adr-001-generated-confext-configuration.md`.

Katl produces and maintains KatlOS, a systemd-native Kubernetes node OS.
Supported domains must compile from user-supplied Katl YAML or configuration
into native systemd/Linux artifacts or bounded Katl-owned files. Katl does not
become a general-purpose host configuration language, a Talos patch
compatibility layer, or a Kubernetes distribution.

## Decision

Katl supports a small set of explicit node configuration domains. Each domain
owns its input shape, render targets, validation, apply behavior, and golden
tests before it becomes user-facing.

Initial supported domains:

```text
kernel command line
  operator-owned additions to the installed kernel arguments
  Katl retains root, immutable-runtime, generation identity, and recovery policy
  changes are staged in a next-boot generation

node identity
  hostname
  stable node name used by generated kubeadm input
  rendered to bounded Katl/systemd host identity files

resolved and host DNS
  only when needed for generation 0 install/runtime or explicit cluster bootstrap
  resolved.conf snippets and host resolver policy
  rendered under /etc/systemd/resolved.conf.d/ or bounded Katl-owned files

sysctl
  Katl-owned kernel parameters needed for Kubernetes and node operation
  rendered under /etc/sysctl.d/

modules-load
  kernel modules needed by supported runtime behavior
  rendered under /etc/modules-load.d/

tmpfiles
  required directories, modes, and ownership for Katl-managed node state
  rendered under /etc/tmpfiles.d/

sysfs
  public hostConfiguration accepts typed name/value settings
  Katl renders exact writes to an internal tmpfiles.d rule

native host configuration
  named file sets below validated /etc paths
  a built-in networkd handler accepts .link, .netdev, .network, and their
    one-level .d/*.conf drop-ins below /etc/systemd/network
  networkd paths are next-boot-only and use installed boot health

mount units
  persistent state projections and disk- or partition-backed volumes
  volume paths and GPT labels are convention-derived from the volume name
  disk initialization uses a Go-planned systemd-repart transition
  rendered as native .mount units and applicable on a running node

Bootstrap profile input
  selected bootstrap profile reference
  implementation resolves the profile to native kubeadm and kubelet
  configuration YAML plus patches rendered under /etc/katl/kubeadm/<name>/

Bootstrap node metadata
  non-secret Katl node metadata rendered to /etc/katl/node.json
  authoritative readiness-probe source for node identity, systemRole, selected
  bootstrap profile, rendered kubeadm input path, and selected Kubernetes
  payload version
  selected Kubernetes sysext payload version used for validation

SSH and operator access
  Katl-managed operator SSH keys
  bounded sshd policy owned by Katl
  no general host account, PAM, sudo, passwd, shadow, or sysusers passthrough

volume requests
  disk- or partition-backed data volumes named by the operator
  Katl derives u-<name> and /var/mnt/<name>
  explicit filesystem and per-target preservation or wipe policy
  Go plans target safety; systemd-repart and native mount units execute it
```

Domains may preserve native file syntax when that is the least lossy interface.
For example, native host configuration accepts systemd-networkd unit content,
but Katl validates and constrains it to `/etc/systemd/network`. Users do not
choose another destination for network files.

## Explicit Deferrals

The following behavior is not part of the initial Katl node configuration API:

```text
BIRD or other routing daemon service packaging
CNI installation and lifecycle
gVisor, Kata, or alternate CRI/runtime integration
storage stack add-ons beyond explicit Katl volumes
GPU, device plugin, or workload accelerator configuration
Katl day-2 update controllers
Kubernetes add-ons, GitOps controllers, ingress, and workload policy
general systemd unit passthrough
general package installation
arbitrary /etc patching
Talos patch compatibility
```

These can be added later as day-2 sysexts, user-managed GitOps, or explicit Katl
domains with their own design and tests. They must not enter through a generic
patch layer.

The opt-in platform API endpoint routing capability is the current durable
example for this boundary. A proposal is documented in
`docs/internal/platform-api-endpoint-routing-capability.md`; it must use the
generic node app sysext contract in
`docs/internal/node-app-sysext-contract.md`, but it still does not make BIRD or
API VIP advertisement part of the initial node configuration API.
The helper's future typed input schema is documented in
`docs/internal/platform-api-endpoint-helper-input-schema.md` and remains bounded
to Katl-owned render paths.

## Validation Expectations

Every supported domain must fail closed on invalid or unsafe input.

Common validation requirements:

```text
reject unknown domains
reject unsupported fields inside known domains
reject path traversal and absolute user-selected render paths
reject symlinks and non-regular source files where files are copied
reject duplicate normalized output paths
reject writes outside the generated confext root
reject writes to /etc/kubernetes
reject host account, PAM, sudo, passwd, shadow, and sysusers ownership
reject runtime-only paths such as /run and generated Katl generation paths
reject conflicting domains that render the same output
reject unsupported sysext selection requests
validate native syntax enough to catch unsupported or dangerous fields
```

Domain-specific expectations:

```text
kernel command line
  require one whitespace-free argument per list entry
  reject duplicate entries and Katl-owned boot arguments
  preserve image-required arguments across install, config apply, and upgrade

node identity
  validate hostname and node name as DNS-compatible single labels or explicitly
  supported fully qualified names
  reject names that conflict with generated kubeadm nodeRegistration
  golden tests cover hostname rendering and kubeadm node-name propagation

resolved and host DNS
  validate bounded resolved settings and DNS server/search-domain values
  golden tests cover generated resolved drop-ins

sysctl
  public operator settings use ADR-011 native sysctl.d file sets
  internal Katl defaults remain bounded product policy

modules-load
  validate module names as plain identifiers or kernel module path segments
  golden tests cover required Kubernetes/network modules

tmpfiles
  keep internal directory and ownership rules limited to Katl-managed paths

sysfs
  accept unique normalized names below /sys and non-empty single-line values
  reject operator-authored tmpfiles.d files
  apply and read-back verify settings before boot health succeeds

native host configuration
  validate bounded /etc ownership, modes, sources, and duplicate paths
  for /etc/systemd/network allow only .link, .netdev, and .network units or
    exactly one matching .d directory containing .conf drop-ins
  reject unsafe names, deeper nesting, unsupported suffixes, and empty network
    content
  golden tests cover shared units plus node-specific drop-ins

mount units and volumes
  derive mount points below /var/mnt and GPT labels u-<name> from validated unique names
  validate filesystem choices, preservation state, and destructive-install guards
  require exactly one typed disk or partition selector
  verify generated units with systemd-analyze verify where practical

Bootstrap profile input
  parse native multi-document kubeadm YAML
  allow kubelet configuration only as native kubelet documents referenced by
  the resolved KubeadmConfig
  reject denied host paths and unsafe patch directories
  supply kubernetesVersion from spec.kubernetes.version and reject conflicts
  default the containerd CRI socket and empty control-plane registration taints
  enforce the writable-state kubelet volumePluginDir
  inject controlPlaneEndpoint and its hostname certificate SAN at bootstrap
  golden tests cover init, join, kubelet configuration, patches, and selected
  sysext mismatch

SSH and operator access
  validate SSH public key syntax
  render only Katl-owned authorized_keys and bounded sshd policy
  reject account database, PAM, sudo, and sysusers changes
```

## Apply Expectations

Install-time materialization writes the selected domains into the generated
confext for the candidate generation. Later runtime configuration changes
should render a new generated confext generation and select it atomically with
the runtime root and sysext set.

Runtime configuration apply receives Katl YAML/configuration through `katlc` and
compiles it on the installed node into generation-scoped artifacts. The
node-local renderer owns the generated confext tree or image, compatibility
validation, generation spec/status, and sysext activation selection. Users do not
hand the node arbitrary confext images or raw extension activation paths as the
configuration API.

`katlc` and KatlOS runtime services must reject unknown and unsupported config
before the renderer writes a generation. Rejection is required for unknown domains,
unsupported fields inside a known domain, unsupported apply modes, unsupported
sysext selection requests, and raw confext or sysext activation paths.

The online versus next-boot runtime apply contract is defined in
`docs/internal/adrs/adr-002-live-and-next-boot-config-apply-modes.md`. Domain
implementations must declare whether each diff is online-applicable,
next-boot-only, operation-only, or rejected before `katlc` and KatlOS runtime
services accept it. The default `auto` apply mode prefers online in-place apply
only when every changed domain has a tested live plan; otherwise it accepts a
next-boot generation or rejects with the required operation kind.

Normal confext activation must not run kubeadm, kubectl, CNI installers, package
managers, or application controllers. Kubeadm-aware actions remain explicit
operator or test-harness steps.

Runtime apply behavior is domain-specific:

```text
kernel command line
  stage a next-boot generation and require reboot before the change is active

networkd host files
  stage a next-boot generation; systemd-networkd and operator access
  participate in installed boot health

resolved
  reload or restart systemd-resolved only through tested KatlOS runtime logic

sysctl
  apply through systemd-sysctl or bounded sysctl calls

modules-load
  apply on boot through systemd-modules-load; runtime loading requires an
  explicit tested path

tmpfiles
  apply through systemd-tmpfiles for Katl-owned paths

mount units
  apply through systemd unit reload/start ordering with rollback checks

Bootstrap profile input
  render desired kubeadm/kubelet input only
  do not mutate live /etc/kubernetes, kube-system ConfigMaps, or
    /var/lib/kubelet files
  if desired input differs from live state, report explicit kubeadm-aware
    action required instead of treating normal config apply as reconciliation

SSH and operator access
  reload sshd only after validation proves access will not be locked out
```

## Testing Contract

Each domain needs deterministic tests before it is considered supported:

```text
unit tests for validation and normalization
golden tests for rendered native artifacts
duplicate-path and denied-path tests
systemd-analyze verify for generated units where practical
systemd-tmpfiles verification for tmpfiles rules where practical
integration or VM smoke tests when apply behavior affects boot, networking,
  storage, kubeadm readiness, or operator access
```

Generated artifacts must be stable across runs. Tests should compare normalized
paths, modes, and content instead of relying on host-specific absolute paths.

## Non-Goals

Katl will not implement Talos patch compatibility. Users who need templating or
large policy overlays can generate Katl input outside Katl, but the committed
Katl API remains explicit domains that compile to native artifacts.

Katl will not use this API to install cluster add-ons or own Kubernetes
lifecycle. Cluster bootstrap may use this API to create kubeadm-ready candidate
generations, but kubeadm and user-managed GitOps own the cluster after bootstrap
succeeds.
