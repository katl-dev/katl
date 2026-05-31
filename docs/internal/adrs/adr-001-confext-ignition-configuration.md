# ADR-001: Katl-native configuration becomes generated confext without Ignition

## Status

Accepted

## Context

Katl is a systemd-native Kubernetes node OS builder. The installed runtime OS is
a pared down Fedora-derived system for kubeadm/kubelet, not a bespoke Linux
distribution and not a managed Kubernetes distribution.

Katl needs to configure a node across three phases:

```text
build time
  katlc and mkosi produce installer and runtime artifacts

install time
  katlos-install prepares the target disk and writes the first node generation

runtime
  a future Katl agent applies later configuration generations
```

Earlier design iterations considered using Ignition for the live installer
environment or first-boot provisioning. That would add another provisioning
language and another boot phase:

```text
Katl config
  -> rendered Ignition
  -> installer applies some state
  -> first boot runs Ignition
  -> runtime activates sysext/confext
  -> node reaches kubeadm-ready state
```

The current direction no longer uses Ignition. The installer replaces the small
amount of installer-environment setup that Ignition would have handled:

```text
receive install input
configure enough network for artifact fetches and local handoff
validate manifests and artifacts
prepare the target disk
write the runtime root artifact
materialize the first generated confext
install boot metadata
reboot into the installed runtime OS
```

Katl also should not require users to supply prebuilt confext images. Confext is
an internal runtime mechanism and artifact boundary. Users should supply
Katl-native configuration and native file content; Katl turns that into a
confext tree or image.

At the same time, Katl should avoid a Talos-style rich machine configuration
schema that hides native Linux/systemd configuration. Katl should not model
every option in `systemd-networkd`, `sshd`, `containerd`, `kubelet`, `kubeadm`,
`resolved`, `timesyncd`, `sysctl`, `modules-load`, and related systems.

Katl needs a thin model:

```text
User supplies install metadata and native configuration content.
katlos-install turns initial node configuration into a generated confext.
The runtime OS activates that confext at boot.
A later Katl runtime agent turns subsequent user configuration into new confext
  generations and applies them.
```

## Decision

Katl will not use Ignition in the core install or runtime configuration path.

Katl will use a Katl-native install manifest as the initial provisioning
contract between generated assets, the installer UKI, `katlos-install`, and the
installed runtime OS.

Users will not be required to build or provide confext images directly. Users
will provide generic-shaped Katl configuration containing:

```text
install metadata
node identity and matching metadata
artifact references and digests
target root disk selection and destructive install authorization
SSH access policy
sysext selections
native /etc file content
bootstrap metadata
```

For the initial install, `katlos-install` will build a node-local generated
confext tree or image from the install manifest and stage it into the installed
runtime OS before first boot.

For later configuration changes, a Katl runtime agent will perform the same
logical operation on the running node: validate user-supplied configuration,
materialize a new generated confext generation, activate it through systemd
primitives, and record generation metadata. The runtime agent can come later,
but the install-time format must leave room for it.

Generated confext content must be generation-scoped. It must not be a single
global `/var/lib/katl/confext/current` tree that can drift independently from
the selected root slot, sysext set, and boot metadata.

## Consequences

### Positive consequences

This removes Ignition from the core architecture.

The install flow becomes:

```text
Katl config
  -> katlc renders build inputs, manifests, and metadata
  -> mkosi builds the installer UKI and runtime artifacts
  -> installer UKI boots
  -> katlos-install receives an install manifest
  -> katlos-install lays out disk, writes runtime, and builds initial confext
  -> reboot
  -> runtime OS activates sysext/confext
  -> node reaches kubeadm-ready state
```

This avoids debugging a three-phase model of installer, Ignition, and runtime
first-boot provisioning.

Katl remains a thin systemd-native OS generator instead of becoming a rich
machine-configuration API. Networking, for example, can be supplied as native
`systemd-networkd` files:

```yaml
etc:
  files:
    "/etc/systemd/network/10-lan.network": |
      [Match]
      Name=enp1s0

      [Network]
      Address=10.1.1.21/24
      Gateway=10.1.1.1
      DNS=10.1.53.1
```

Katl only needs to validate allowed paths, ownership, modes, and optionally run
native verification tools. It does not need to model every possible
`systemd-networkd` feature.

The user experience is:

```text
I define the node.
Katl installs it.
The runtime OS boots with that config.
Later, a Katl agent can apply config updates through new confext generations.
```

### Negative consequences

Katl must own more installer and runtime-agent logic.

The installer must be able to:

```text
parse and validate Katl install manifests
receive install input without Ignition
materialize native file bundles
build a confext-compatible tree or image
write extension metadata
stage the generated confext in generation-scoped state
configure active extension pointers
write first-boot and generation state
validate the target installation
```

The future runtime agent must eventually be able to:

```text
validate user-supplied runtime configuration
materialize new confext generations
coordinate confext activation with systemd
record generation metadata
support rollback to a previous generated config set
avoid mutating kubeadm-owned state
```

Katl cannot delegate those concerns to Ignition.

Existing matchbox/Ignition workflows cannot be reused directly. User-managed
PXE/iPXE/matchbox remains useful for booting the installer UKI and passing or
serving install manifest input, but Katl does not own that provisioning layer.

There is less compatibility with the Fedora CoreOS/Flatcar provisioning
ecosystem. Katl is choosing a project-specific install contract over an existing
provisioning format.

Because user configuration becomes generated `/etc`, manifests and later config
updates are security-sensitive. They must come through a trusted Katl input path
before they are materialized into confext. Generated confexts do not need to be
signed in the default path because they are produced on the target node from
already trusted input. Signing generated confexts can be added later if it
proves useful.

## Detailed Design

## Install Input

The install manifest is the initial provisioning artifact. It may be supplied
by any installer input path supported by `katlos-install`:

```text
kernel command line or boot metadata
local file bundled next to the installer material
local handoff HTTP API
user-managed network boot metadata
```

Katl does not require or render Ignition for these paths.

## Example Install Manifest Shape

```yaml
apiVersion: install.katl.dev/v1alpha1
kind: InstallManifest
metadata:
  nodeName: ms01-01
spec:
  match:
    mac:
      - "00:11:22:33:44:55"

  install:
    disk: /dev/disk/by-id/nvme-Samsung_...
    wipe: true

  runtime:
    root:
      ref: https://assets.example/katl-runtime-v0.1.0.squashfs
      format: squashfs
      sizeBytes: 123456789
      digest: sha256:...
    boot:
      ukiDigest: sha256:...

  sysext:
    - name: kubernetes
      ref: https://assets.example/kubernetes-v1.34.2.raw
      digest: sha256:...

  bootstrap:
    mode: manual

  etc:
    files:
      "/etc/hostname": |
        ms01-01

      "/etc/systemd/network/10-lan.network": |
        [Match]
        Name=enp1s0

        [Network]
        Address=10.1.1.21/24
        Gateway=10.1.1.1
        DNS=10.1.53.1

      "/etc/ssh/sshd_config.d/10-katl.conf": |
        PasswordAuthentication no
        PermitRootLogin no

      "/etc/katl/kubeadm-init.yaml": |
        apiVersion: kubeadm.k8s.io/v1beta4
        kind: ClusterConfiguration
        kubernetesVersion: v1.34.2
        controlPlaneEndpoint: 10.45.0.10:6443
```

The manifest is intentionally generic. Katl understands install mechanics,
artifact references, artifact verification, and file placement. Katl does not
deeply understand every subsystem represented by file contents.

The target root disk is selected by the user and then owned by Katl. Users do
not configure the root disk partition table, root slot sizes, root slot
filesystem format, state partition filesystem, partition labels, or alignment.
Those are Katl implementation details controlled by the installer/runtime
profile. Extra non-root data disks can have a separate explicit policy later.

## Installer Responsibilities

`katlos-install` consumes the install manifest and performs the installation.

It must:

```text
identify or select the node
verify the manifest
verify referenced artifacts
apply Katl's target disk layout
create filesystems on the real target disk
write the prebuilt SquashFS runtime root artifact into root-a
install boot metadata for the selected generation
install or stage sysext artifacts
materialize /etc file bundle into a generated confext tree or image
write confext extension metadata
stage the confext into generation-scoped installed state
write Katl node and generation state
write first-boot marker
reboot
```

The installer does not run Ignition.

The installer does not run kubeadm by default.

The installer does not install Cilium, CoreDNS, Rook, Flux, or workloads.

## Confext Construction

The installer builds the first generated confext from the manifest's `etc.files`
section.

Conceptual generation-scoped staging layout:

```text
/target/var/lib/katl/generations/<generation-id>/
  manifest.json
  metadata.json
  confext/
    etc/
      hostname
      systemd/
        network/
          10-lan.network
        system/
          ...
      ssh/
        sshd_config.d/
          10-katl.conf
      katl/
        kubeadm-init.yaml
      extension-release.d/
        extension-release.katl-node
```

The extension metadata should identify compatibility with the runtime OS
generation, for example:

```text
ID=katl
VERSION_ID=0.1.0
CONFEXT_LEVEL=1
```

The generated confext may initially be a directory tree. Later, Katl may package
it as a raw confext image with verity/signature support.

The runtime activates only the selected generation's confext using
`systemd-confext`. Activation should be coordinated with the selected root slot,
boot metadata, and sysext set.

## Runtime Agent Responsibilities

A future Katl runtime agent will reuse the same configuration model after first
boot. It is not required for the first installer milestone, but the initial
layout must leave room for it.

The agent should eventually:

```text
receive or discover desired Katl configuration
validate input trust and policy
render native file content into a new confext generation
verify generated file paths, modes, ownership, and metadata
stage the confext under /var/lib/katl/generations/<generation-id>/
activate the new generation through systemd-confext
record success or failure
support rollback to a previous generated config set
```

The agent should not become a general-purpose configuration management system.
It applies Katl-generated configuration generations and preserves native
systemd/Linux file semantics.

## `/etc` Ownership Boundaries

Katl-generated steady-state config should live in the generated confext.

Examples:

```text
/etc/hostname
/etc/hosts
/etc/systemd/network/*.network
/etc/systemd/network/*.netdev
/etc/systemd/resolved.conf.d/*.conf
/etc/systemd/timesyncd.conf.d/*.conf
/etc/ssh/sshd_config.d/*.conf
/etc/containerd/config.toml
/etc/sysctl.d/*.conf
/etc/modules-load.d/*.conf
/etc/modprobe.d/*.conf
/etc/katl/*
```

Kubeadm-generated mutable state must not be placed in confext.

Kubeadm should own:

```text
/etc/kubernetes/pki
/etc/kubernetes/*.conf
/etc/kubernetes/manifests
```

Because the runtime root is read-only, `/etc/kubernetes` needs an explicit
persistent projection from writable state, such as a systemd bind mount sourced
from `/var/lib/katl/kubernetes` or another validated state path. It must not be
made part of the generated confext.

Katl should place kubeadm input config under:

```text
/etc/katl/kubeadm-init.yaml
/etc/katl/kubeadm-join.yaml
```

Then kubeadm is invoked as:

```bash
kubeadm init --config /etc/katl/kubeadm-init.yaml
```

or:

```bash
kubeadm join --config /etc/katl/kubeadm-join.yaml
```

## Runtime Boot

The runtime OS boots with the selected generation's confext already present.

Runtime flow:

```text
mount filesystems, including writable state at /var
ensure stable machine identity is available
activate sysext
activate selected generation confext
project writable /etc/kubernetes from state
start systemd-networkd
start sshd
start containerd
start kubelet
reach katl-kubeadm-ready.target
```

A small first-boot service may exist, but it is not a general provisioning
system.

It may:

```text
validate Katl install state
ensure active extension links exist
generate or persist SSH host keys if absent
mark first boot complete
```

It should not be responsible for writing arbitrary configuration files. Initial
configuration was materialized by `katlos-install`; later configuration changes
belong to the Katl runtime agent.

## Network Boot Path

Katl does not own how a machine reaches the installer. Users may use
PXE/iPXE/matchbox, a mounted ISO, a remote KVM workflow, or another boot
process.

Generic flow:

```text
user-managed boot infrastructure boots the Katl installer UKI
boot metadata or local handoff supplies the Katl install manifest
katlos-install applies the manifest
katlos-install reboots into the installed runtime OS
```

No Ignition config is required.

## Local Handoff Path

If no manifest is preseeded, `katlos-install` may start a small HTTP handoff
server in the installer environment. The operator or test harness submits the
same install manifest used by preseeded installs.

The handoff API is only for initial installer input. It is not the runtime
configuration agent and must stop accepting config once installation starts.

## Security

Because user configuration defines files that become `/etc`, it is
security-sensitive.

The installer and future runtime agent must verify:

```text
manifest or configuration trust according to active Katl policy
artifact digests
artifact signatures
allowed file paths
file modes and ownership
symlink behavior
no path traversal
no writes outside allowed confext root
generation metadata consistency
```

Generated confexts do not require their own signatures in the default path.
They are generated locally from trusted manifest/configuration input and tracked
through generation metadata. Later support for signed generated confext images is
an optional hardening or distribution improvement, not a requirement for this
ADR.

File paths in `etc.files` must be absolute paths under `/etc`.

The installer and runtime agent should reject:

```text
paths containing ..
paths outside /etc
unexpected symlinks
device nodes
setuid files unless explicitly allowed
dangerous ownership/mode combinations unless explicitly allowed
attempts to own kubeadm mutable output under /etc/kubernetes
```

SSH access should default to key-only auth.

Secrets should not be stored in plain text in normal manifests unless the user
explicitly accepts that risk. Future work may add encrypted configuration
sections using SOPS/age or TPM-sealed material.

## Alternatives Considered

## Alternative 1: Use Ignition

Rejected for the core path.

Ignition would be useful if the runtime OS were a generic cloud-style image that
self-provisions entirely on first boot. Katl has a real installer and will have
a runtime agent for later configuration generations. Ignition would add another
configuration language and boot phase without enough benefit.

Ignition may be considered later only as an import/export compatibility target,
not as a mechanism required by the installer or runtime.

## Alternative 2: Users supply prebuilt confext images

Rejected as the primary interface.

Prebuilt confext images are useful for reproducibility, signing, and advanced
workflows, but they are too much complexity for the default user interface.

The default user should supply Katl configuration and native file content, not
extension images.

Katl may later support CI-built signed confext artifacts as an optimization, but
this should not be required for the default install flow.

## Alternative 3: Rich Talos-style machine config

Rejected.

A rich schema would eventually force Katl to model and render many Linux
subsystems. This conflicts with the design goal of using native systemd/Linux
configuration directly.

Katl should not need a new frontend field to support a new
`systemd-networkd` feature.

## Alternative 4: Mutable `/etc` written directly by installer

Rejected as the default.

The installer could write files directly into `/etc`, but that weakens the
generated/immutable model.

Using confext gives Katl:

```text
clear ownership of generated config
generation-scoped replacement of config sets
rollback path
less configuration drift
read-only merged config at runtime
alignment with systemd-native primitives
```

Some paths, especially kubeadm output under `/etc/kubernetes`, remain writable
by design through explicit state projection.

## Final Decision Summary

Katl will use Katl-native install manifests and later Katl-native runtime
configuration instead of Ignition.

Users will provide generic configuration containing install metadata and native
file content. They will not provide confext images directly in the default path.

`katlos-install` will build the first node-local generated confext during
installation.

A future Katl runtime agent will build and apply later generated confext
generations on the running node.

The runtime OS will activate selected confext generations with
`systemd-confext`, coordinated with root slot, sysext, and generation metadata.
