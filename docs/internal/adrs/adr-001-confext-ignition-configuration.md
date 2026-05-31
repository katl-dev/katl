# ADR-001: Katl node provisioning uses Katl-native install manifests and on-node confext construction

## Status

Accepted

## Context

Katl is a systemd-native Kubernetes Cluster OS builder. The installed operating system is Katlos.

Katl targets technically capable home-lab Kubernetes operators running small clusters, typically 1–10 nodes. The goal is to provide a boring, repeatable, recoverable Kubernetes node OS that reaches a deterministic `kubeadm`-ready state. Katl is not intended to be a general-purpose Linux distribution, a Talos clone, or a fully managed Kubernetes distribution.

Earlier design iterations considered using Ignition for first-boot provisioning and prebuilt `systemd-confext` images for node configuration. This would have produced a flow similar to:

```text
Katl config
  -> rendered Ignition
  -> installer writes generic OS
  -> first boot runs Ignition
  -> Ignition writes node config
  -> runtime activates sysext/confext
  -> node reaches kubeadm-ready state
```

However, Katl already requires a real installer. The installer must apply a user-defined disk layout, create filesystems on the actual target disk, install Katlos, seed runtime state, configure boot entries, and install or stage extension artifacts. Because the installer is already responsible for turning a generic runtime into a specific installed node, using Ignition adds another provisioning language and boot phase without enough benefit.

Similarly, exposing confext images directly as the primary user-facing configuration artifact creates unnecessary complexity. Users should not need to understand or build confext images merely to configure hostname, networking, SSH, kubeadm input files, or systemd drop-ins.

At the same time, Katl should avoid the Talos-style approach of defining a rich domain-specific machine configuration schema and internally translating it into low-level Linux/systemd configuration. That approach would force Katl to become an operator/rendering layer for every Linux subsystem it touches: `systemd-networkd`, `sshd`, `containerd`, `kubelet`, `kubeadm`, `resolved`, `timesyncd`, `sysctl`, `modules-load`, and so on.

Katl needs a thinner model:

```text
User supplies install metadata and native file content.
Katl installer materializes those files into a confext-compatible tree on the installed node.
Katlos activates that confext at boot.
```

This keeps Katl’s configuration surface generic while preserving an immutable/generated runtime model.

## Decision

Katl will not use Ignition in the golden path.

Katl will use a Katl-native install manifest as the provisioning contract between generated assets, PXE/ISO boot, the installer, and the installed runtime.

Katl users will not be required to supply prebuilt confext images. Instead, users will provide generic-shaped manifests containing:

* install metadata,
* node identity and matching metadata,
* artifact references,
* disk layout,
* SSH access policy,
* sysext selections,
* native `/etc` file content,
* bootstrap metadata.

The Katl installer will build a node-local confext tree or image from the manifest during installation.

The generated confext will be staged into the installed Katlos system before first boot. When Katlos boots, `systemd-confext` activates the prebuilt local confext, giving the node its generated `/etc` configuration without requiring Ignition.

## Consequences

### Positive consequences

This removes Ignition from the core architecture.

The install flow becomes:

```text
Katl config
  -> Katl build
  -> PXE/ISO assets and install manifests
  -> Katl installer
  -> disk layout + runtime install + sysext install + local confext build
  -> reboot
  -> Katlos activates sysext/confext
  -> node reaches kubeadm-ready state
```

This avoids a separate first-boot provisioning language and avoids debugging a three-phase model of installer, Ignition, and runtime.

Katl remains a thin systemd-native OS generator instead of becoming a rich machine-configuration API.

Networking, for example, can be supplied as native `systemd-networkd` files:

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

Katl does not need to model every possible `systemd-networkd` feature. It only needs to validate that a file is allowed, place it in the right tree, and optionally run native validation tools.

The user experience is simpler:

```text
I define the node.
Katl installs it.
Katlos boots with that config.
```

instead of:

```text
I define the node.
Katl renders Ignition.
Ignition writes files.
Something else turns those files into runtime state.
```

This also lets PXE and ISO paths share the same core install contract. PXE/matchbox can serve per-node install manifests. ISO images can embed all node manifests and allow node selection by MAC address, UUID, DMI serial, or explicit boot argument.

### Negative consequences

Katl must own more installer logic.

The installer must be able to:

* parse and validate Katl install manifests,
* materialize native file bundles,
* build a confext-compatible tree or image,
* write extension metadata,
* stage the generated confext in the installed target,
* configure active extension pointers,
* write first-boot state,
* validate the target installation.

Katl cannot delegate those concerns to Ignition.

Existing matchbox/Ignition workflows cannot be reused directly. Matchbox remains useful for PXE templating, machine matching, and serving boot assets, but it will serve Katl install manifests rather than Ignition configs.

There is less compatibility with the Fedora CoreOS/Flatcar provisioning ecosystem. Katl is explicitly choosing a project-specific install contract over an existing provisioning format.

Prebuilt, signed confext images are not the default artifact in the simple path. If the installer builds the confext locally from a manifest, the manifest must be signed and verified carefully because it effectively defines the node’s `/etc`.

## Detailed design

## Katl install manifest

A Katl install manifest is the primary provisioning artifact.

Example:

```yaml
apiVersion: katl.dev/v1alpha1
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
    layout:
      partitionTable: gpt
      partitions:
        - name: esp
          type: esp
          size: 1GiB
          filesystem:
            type: vfat
          mount:
            path: /efi

        - name: root
          type: linux-generic
          size: 8GiB
          filesystem:
            type: erofs
          mount:
            path: /

        - name: var
          type: linux-generic
          size: remaining
          filesystem:
            type: ext4
          mount:
            path: /var

  runtime:
    ref: ghcr.io/katl/katlos-runtime:v0.1.0
    digest: sha256:...

  sysext:
    - name: kubernetes
      ref: ghcr.io/katl/sysext/kubernetes:v1.34.2-katl.0
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

The manifest is intentionally generic. Katl understands install mechanics, artifact references, and file placement. Katl does not deeply understand every subsystem represented by the file contents.

## Manifest responsibilities

The manifest may define:

```text
node matching
target disk
disk layout
runtime artifact
sysext artifacts
file bundle for /etc
SSH policy
bootstrap mode
trust policy
first-boot state
```

The manifest should not require Katl to provide rich schemas for:

```text
systemd-networkd
sshd
containerd
kubeadm
kubelet
resolved
timesyncd
sysctl
modules-load
modprobe
```

Native files are the primary escape hatch and the primary configuration mechanism.

## Installer responsibilities

The Katl installer consumes the install manifest and performs the installation.

It must:

```text
identify/select the node
verify the manifest
verify referenced artifacts
apply the target disk layout
create filesystems on the real target disk
install the Katlos runtime
install boot entries
install or stage sysext artifacts
materialize /etc file bundle into a confext-compatible tree
write confext extension metadata
stage the confext into the installed system
write Katl node state
write first-boot marker
reboot
```

The installer does not run Ignition.

The installer does not need to run kubeadm.

The installer does not install Cilium, CoreDNS, Rook, Flux, or workloads.

## Confext construction

The installer builds a confext from the manifest’s `etc.files` section.

Conceptual staging layout:

```text
/target/var/lib/katl/confext/current/
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

The extension metadata should identify compatibility with the Katlos runtime, for example:

```text
ID=katlos
VERSION_ID=0.1.0
CONFEXT_LEVEL=1
```

The generated confext may initially be a directory tree. Later, Katl may package it as a raw confext image with verity/signature support.

The runtime activates this local confext during boot using `systemd-confext`.

## `/etc` ownership boundaries

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

This avoids making `/etc/kubernetes` read-only or confext-owned.

## Runtime boot

Katlos boots with the confext already present.

Runtime flow:

```text
mount filesystems
ensure machine-id exists
activate sysext
activate confext
start systemd-networkd
start sshd
start containerd
start kubelet
reach katl-kubeadm-ready.target
```

A small `katl-firstboot.service` may exist, but it is not a general provisioning system.

It may:

```text
ensure machine-id exists
generate SSH host keys if absent
validate Katl install state
ensure active extension links exist
mark first boot complete
```

It should not be responsible for writing arbitrary configuration files. That happened during install.

## PXE/matchbox path

Matchbox remains the preferred PXE path, but it serves Katl assets instead of Ignition.

Flow:

```text
node PXE boots
matchbox matches node by MAC/UUID
matchbox serves iPXE profile
iPXE boots Katl installer
installer fetches Katl install manifest
installer applies manifest
installer reboots into Katlos
```

Matchbox may serve:

```text
installer kernel/initrd/UKI
install manifest
runtime artifact references
sysext artifact references
```

No Ignition config is required.

## ISO path

The ISO contains:

```text
Katl installer
Katlos runtime artifact
sysext artifacts
all node install manifests
node selection metadata
```

Flow:

```text
boot ISO
select node by cmdline, MAC, UUID, DMI serial, or interactive prompt
installer applies selected manifest
installer builds local confext
installer reboots into Katlos
```

No Ignition stage is required.

## Security

Because the manifest defines files that will become `/etc`, it is security-sensitive.

The installer must verify:

```text
manifest signature
artifact digests
artifact signatures
allowed file paths
file modes and ownership
symlink behavior
no path traversal
no writes outside allowed confext root
```

File paths in `etc.files` must be absolute paths under `/etc`.

The installer should reject:

```text
paths containing ..
paths outside /etc
unexpected symlinks
device nodes
setuid files unless explicitly allowed
dangerous ownership/mode combinations unless explicitly allowed
```

SSH access should default to key-only auth.

Secrets should not be stored in plain text in normal manifests unless the user explicitly accepts that risk. Future work may add encrypted manifest sections using SOPS/age or TPM-sealed material.

## Alternatives considered

## Alternative 1: Use Ignition

Rejected for the golden path.

Ignition would be useful if Katlos were a generic cloud-style image that self-provisions entirely on first boot. Katl instead has a real installer that already applies disk layout and materializes node state. Ignition would add a second provisioning contract and another boot phase.

This would make the flow more complex:

```text
Katl manifest
  -> Ignition config
  -> installer seed
  -> first boot provisioning
  -> confext activation
```

The chosen model is simpler:

```text
Katl manifest
  -> installer materializes installed state
  -> confext activation
```

Ignition may be considered later as an import/export compatibility target, but not as a core mechanism.

## Alternative 2: Users supply prebuilt confext images

Rejected as the primary interface.

Prebuilt confext images are useful for reproducibility, signing, and advanced workflows, but they are too much complexity for the default UX.

The default user should supply native files and metadata, not extension images.

Katl may later support:

```text
katl build confext
```

or CI-built signed confext artifacts, but this should be an optimization, not a requirement.

## Alternative 3: Rich Talos-style machine config

Rejected.

A rich schema would eventually force Katl to model and render many Linux subsystems. This conflicts with the design goal of using native systemd/Linux configuration directly.

Katl should not need a new frontend field to support a new `systemd-networkd` feature.

## Alternative 4: Mutable `/etc` written directly by installer

Rejected as the default.

The installer could simply write files directly into `/etc`, but that weakens the generated/immutable model.

Using confext gives Katl:

```text
clear ownership of generated config
atomic-ish replacement of config sets
rollback path
less configuration drift
read-only merged config at runtime
alignment with systemd-native primitives
```

Some paths, especially kubeadm output under `/etc/kubernetes`, remain writable by design.

## Implementation notes

Initial implementation can use a directory-based confext rather than a raw disk image.

Example active layout:

```text
/var/lib/katl/confext/
  current/
    etc/
      ...
  previous/
    etc/
      ...
```

A later implementation can produce:

```text
/var/lib/katl/confext/
  katl-node-v1.confext.raw
  katl-node-v2.confext.raw
```

The local confext should be treated as generated state. Users should edit the source manifest and reinstall/rebuild rather than editing the generated tree directly.

A future `katlctl` may support:

```bash
katlctl status
katlctl diff-confext
katlctl activate-confext previous
katlctl rebuild-confext /path/to/manifest
```

## Final decision summary

Katl will use a Katl-native install manifest rather than Ignition.

Katl users will provide generic manifests containing install metadata and native file content.

The Katl installer will build a node-local confext from those native files during installation.

Katlos will activate that confext at runtime using `systemd-confext`.

This keeps Katl simple, avoids a Talos-style rendering engine, avoids Ignition complexity, and preserves a systemd-native immutable runtime model.
