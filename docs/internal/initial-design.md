# Technical Design: Katl, a systemd-native Kubernetes Cluster OS Builder

Status note: this is an early working design and still contains historical
exploration. For installer/runtime component direction, prefer
`docs/internal/installer-runtime-design.md` and the ADRs under
`docs/internal/adrs/`. In particular, ADR-001 supersedes earlier Ignition
guidance: Katl no longer uses Ignition in the core install or runtime
configuration path. `katlos-install` handles initial installer input and builds
the first generated confext; a later runtime agent will apply subsequent
configuration generations.

## 1. Summary

Katl builds a Kubernetes-focused operating system generator for home-lab clusters. It is not a general-purpose Linux distribution and it is not a Talos-like minimal custom userspace. It is closer to CoreOS or Flatcar in operational model, but scoped specifically around producing Kubernetes-ready nodes using modern systemd primitives.

The system compiles high-level cluster and node configuration into:

* Installer UKI and later wrapper assets.
* Install manifests.
* Runtime OS images.
* `systemd-sysext` images for host components.
* Generated `systemd-confext` configuration generations.
* Optional recovery assets.

The primary user-facing tool is `katlc`, the Katl compiler. Given a Katl configuration repository, `katlc` should produce bootable installer assets and update artifacts that can be published to GitHub Releases, an OCI registry, an object store, or a matchbox asset directory.

The runtime system should boot into a state where the node is ready to run `kubeadm init` or `kubeadm join`.

The project boundary is deliberately:

```text
Installer + runtime OS prepares a kubeadm-ready node.
kubeadm creates or joins the Kubernetes cluster.
Kubernetes add-ons such as Cilium, CoreDNS, Rook, Flux, monitoring, and workloads are installed afterwards by the user or GitOps.
```

## 2. Target users

The target users are technically capable home-lab Kubernetes operators.

Typical users:

* Run 1–10 physical or virtual nodes.
* Currently use Talos, Flatcar, Fedora CoreOS, Debian, Ubuntu, NixOS, or hand-built nodes.
* Want boring, repeatable, recoverable Kubernetes nodes.
* Are comfortable editing YAML, systemd-networkd units, native Linux/systemd files, and Git repositories.
* Prefer generated infrastructure over click-driven installers.
* Want immutable-ish OS operation without losing access to familiar Linux/systemd tools.
* Want PXE installs for bare metal, but also want USB/ISO installs for ad-hoc or recovery workflows.

This project should not aim at users who want a fully managed Kubernetes appliance with no Linux knowledge.

## 3. Design goals

## 3.1 Primary goals

1. Build a Kubernetes node operating system using mkosi and systemd-native primitives.
2. Support user-managed network boot and later USB/ISO installs without owning the provisioning layer.
3. Use Katl-native install manifests and generated confext for node configuration rather than Ignition.
4. Use `systemd-sysext` for host component extensions.
5. Use `systemd-confext` for `/etc` configuration overlays.
6. Compile high-level cluster/node config into native systemd artifacts with minimal abstraction.
7. Avoid schema designs that require new frontend features every time systemd-networkd supports a new networking feature.
8. Install to disks by creating partition tables and filesystems, not by blindly writing a monolithic raw disk image.
9. Handle normal SSD/HDD/NVMe sector geometry differences by using native partitioning/filesystem tooling at install time.
10. Provide SSH access for install, recovery, and optionally runtime break-glass.
11. Keep the installed runtime immutable by default.
12. Provide a repeatable way to build runtime images, sysext images, confext images, installer images, and ISO images.
13. Produce a node that is ready to run `kubeadm init` or `kubeadm join`.

## 3.2 Non-goals

1. Do not replace kubeadm initially.
2. Do not provide a full Kubernetes distribution.
3. Do not bake Cilium, CoreDNS, Rook, Flux, or observability into the base OS.
4. Do not require a package manager on the running node.
5. Do not provide a general-purpose mutable Linux server.
6. Do not invent a full custom init system.
7. Do not build a “seven binary” Talos clone.
8. Do not require a cloud metadata service.
9. Do not make Matchbox mandatory for USB/ISO installs.
10. Do not make the high-level config a lossy abstraction over native systemd config.

## 4. Architecture overview

The project has five main layers:

```text
User configuration
  ↓
katlc config compiler
  ↓
Build pipeline
  ├── installer image
  ├── runtime image
  ├── sysext images
  ├── confext images
  ├── Ignition configs
  ├── matchbox profiles
  └── ISO image
  ↓
Install path
  ├── PXE/iPXE via matchbox
  └── USB/ISO
  ↓
Installed runtime
  ├── immutable base OS
  ├── sysext host components
  ├── confext /etc config
  ├── SSH access
  ├── container runtime
  ├── kubelet/kubeadm/kubectl
  └── kubeadm-ready target
```

The system produces two separate operating environments:

1. **Installer environment**
   A temporary live environment used to install the runtime OS onto a target disk.

2. **Runtime environment**
   The installed node OS that boots from disk and prepares the node for kubeadm.

## 5. Key design choice: installer is not `dd`

The installer must not simply write a full raw disk image to a disk.

Instead, installation should:

1. Discover the target disk.
2. Validate the disk against the install manifest.
3. Wipe existing signatures if allowed.
4. Create a GPT partition table.
5. Create partitions according to the user-defined disk layout.
6. Format filesystems.
7. Install the runtime root filesystem or image contents into the target layout.
8. Install bootloader or UKI entries.
9. Write Ignition/first-boot seed data.
10. Optionally preload sysext/confext artifacts.
11. Reboot.

This avoids coupling install success to the block-device mechanics of a prebuilt raw image. It also makes the installer suitable for different NVMe/SATA/SAS devices, different logical/physical sector sizes, and different capacity drives.

The generated runtime image can still be used as an input artifact, but the installer should apply it into a real disk layout rather than blindly cloning it.

## 6. Component model

## 6.1 Installer image

The installer image is allowed to be larger and more convenient than runtime.

It should include:

```text
kernel
initramfs
systemd
udev
journald
systemd-networkd
systemd-resolved or static resolver support
CA certificates
SSH server
installer agent
Ignition support or Ignition fetch/apply helper
disk discovery tools
partitioning tools
filesystem creation tools
mount/umount/findmnt
blkid/lsblk/wipefs
systemd-repart, if used
bootctl or bootloader installer
sbsigntools, optional
curl
jq/yq, optional
artifact fetcher
signature verifier
debug tools
```

The installer is responsible for:

```text
network bootstrap
node identification
manifest retrieval
manifest verification
disk layout application
runtime installation
Ignition seed installation
sysext/confext preloading
bootloader/UKI setup
reboot
```

The installer is not responsible for:

```text
running Kubernetes
running kubeadm
running kubelet as a real node service
installing CNI
joining the cluster
performing steady-state updates
```

## 6.2 Runtime base OS

The runtime base OS should be small, stable, and boring.

It should include:

```text
kernel
initramfs
systemd
udev
journald
systemd-networkd
systemd-resolved or static resolver support
systemd-timesyncd or chrony
systemd-sysext
systemd-confext
systemd-tmpfiles
systemd-sysctl
systemd-modules-load
CA certificates
basic util-linux
iproute2
kmod
SSH server, optional but recommended for this audience
node agent
extension manager
container runtime, initially recommended in base
runc or crun
```

The runtime base should not include:

```text
Cilium
CoreDNS
Rook
Flux
Helm
Prometheus
general package manager
build toolchain
large debug suite
application workloads
```

## 6.3 Kubernetes sysext

The Kubernetes sysext should contain:

```text
/usr/bin/kubelet
/usr/bin/kubeadm
/usr/bin/kubectl
/usr/lib/systemd/system/kubelet.service
/usr/lib/systemd/system/katl-kubeadm-ready.target
/usr/lib/systemd/system/katl-kubeadm-init.service
/usr/lib/systemd/system/katl-kubeadm-join.service
```

It may also include:

```text
crictl
kubeadm helper scripts
preflight helper
```

Recommended artifact naming:

```text
kubernetes-v1.34.2.sysext.raw
```

Kubernetes version changes become sysext changes.

## 6.4 Optional sysexts

Additional sysexts may provide:

```text
debug tools
storage tools
network routing tools
FRR
BIRD
nvme-cli
smartmontools
bpftool
tcpdump
ethtool
```

These should be separate from the core Kubernetes sysext.

## 6.5 confext

Configuration extensions own `/etc`.

Common confext contents:

```text
/etc/hostname
/etc/hosts
/etc/systemd/network/*.network
/etc/systemd/network/*.netdev
/etc/systemd/resolved.conf.d/*.conf
/etc/systemd/timesyncd.conf.d/*.conf
/etc/sysctl.d/*.conf
/etc/modules-load.d/*.conf
/etc/modprobe.d/*.conf
/etc/containerd/config.toml
/etc/kubernetes/kubelet-config.yaml
/etc/kubernetes/kubeadm-init.yaml
/etc/kubernetes/kubeadm-join.yaml
/etc/systemd/system/*.d/*.conf
/etc/ssh/sshd_config.d/*.conf
/etc/katl/node.yaml
```

The runtime should treat confext as the primary source of `/etc` configuration. Any mutable `/etc` state should be either:

* explicitly allowed,
* generated at first boot,
* stored in a persistent writable state area and bind-mounted if needed,
* or created by kubeadm as part of cluster bootstrap.

## 7. Configuration model

## 7.1 Thin abstraction principle

The frontend config should avoid re-modeling all of systemd.

For example, networking should not become a project-specific schema that needs new fields every time systemd-networkd supports a new feature.

Instead, the config model should support two layers:

1. **Convenience fields** for common simple cases.
2. **Native passthrough** for full systemd-networkd content.

Example:

```yaml
network:
  files:
    "10-lan.network": |
      [Match]
      Name=enp1s0

      [Network]
      Address=10.1.1.21/24
      Gateway=10.1.1.1
      DNS=10.1.53.1
      DNS=10.1.53.2

    "20-k8s-ptp.network": |
      [Match]
      Name=enp4s0f0np0

      [Network]
      Address=10.254.1.1/31
```

A convenience layer may exist:

```yaml
network:
  interfaces:
    - match:
        name: enp1s0
      addresses:
        - 10.1.1.21/24
      gateway: 10.1.1.1
      dns:
        - 10.1.53.1
        - 10.1.53.2
```

But it should compile into the same native file model and users must always be able to bypass it with raw systemd units.

## 7.2 Native artifact sections

The high-level config should allow native files directly:

```yaml
files:
  confext:
    "/etc/systemd/network/10-lan.network": |
      [Match]
      Name=enp1s0

      [Network]
      DHCP=yes

    "/etc/systemd/system/containerd.service.d/10-katl.conf": |
      [Service]
      LimitNOFILE=1048576

  ignition:
    "/etc/ssh/authorized_keys.d/admin": |
      ssh-ed25519 AAAA...
```

This keeps the project from becoming a leaky abstraction over systemd.

## 7.3 Node and cluster config

A minimal cluster config:

```yaml
apiVersion: katl.dev/v1alpha1
kind: Cluster
metadata:
  name: home
spec:
  kubernetes:
    version: v1.34.2
  runtime:
    image: katl-runtime
    profile: ms01
  installer:
    profile: default
  ssh:
    authorizedKeys:
      - ssh-ed25519 AAAA...
  nodes:
    - name: ms01-01
      role: control-plane
      match:
        mac: "00:11:22:33:44:55"
      install:
        disk: /dev/disk/by-id/nvme-Samsung_...
      kubeadm:
        mode: init
    - name: ms01-02
      role: control-plane
      match:
        mac: "00:11:22:33:44:56"
      install:
        disk: /dev/disk/by-id/nvme-Samsung_...
      kubeadm:
        mode: join-control-plane
```

Node-specific config can override or append native files.

## 8. Disk layout model

## 8.1 Requirements

Disk layout must be user-defined but compiled into installer actions.

The installer must support:

```text
GPT partition tables
EFI system partition
root filesystem
persistent state partition or subvolume
optional separate /var
optional separate /var/lib/etcd
optional separate /var/lib/containerd
optional separate /var/lib/kubelet
optional recovery partition
optional encrypted partitions later
```

## 8.2 Recommended default layout

For the initial version:

```text
ESP
  mounted at /efi or /boot
  FAT32
  contains systemd-boot, loader entries, and/or UKIs

XBOOTLDR, optional but recommended if ESP size is a concern
  mounted at /boot
  Linux filesystem
  contains UKIs, loader entries, and boot metadata

root-a
  mounted at /
  immutable runtime root slot
  SquashFS or another read-only filesystem

root-b
  inactive runtime root slot
  SquashFS or another read-only filesystem

state
  mounted at /var
  writable persistent node state
```

For control-plane nodes, strongly consider:

```text
etcd
  mounted at /var/lib/etcd
  writable persistent data
```

A default layout:

```yaml
diskLayout:
  partitionTable: gpt
  partitions:
    - name: esp
      type: esp
      size: 1GiB
      filesystem:
        type: vfat
      mount:
        path: /efi

    - name: boot
      type: xbootldr
      size: 2GiB
      filesystem:
        type: ext4
      mount:
        path: /boot

    - name: root-a
      type: root
      size: 8GiB
      filesystem:
        type: squashfs
      mount:
        path: /
        active: true

    - name: root-b
      type: root
      size: 8GiB
      filesystem:
        type: squashfs
      mount:
        active: false

    - name: var
      type: linux-generic
      size: remaining
      filesystem:
        type: ext4
      mount:
        path: /var
```

The actual schema should permit raw or semi-native partition definitions rather than hiding all systemd-repart capabilities.

The installer should not build the runtime root filesystem on the target node and then squash it. The normal flow should be:

```text
katlc/mkosi build produces a signed runtime root filesystem artifact.
Installer verifies the artifact.
Installer writes or installs that artifact into the inactive or selected root slot.
Boot metadata points systemd-boot at the selected slot.
Runtime mounts the selected root slot read-only.
```

This is still different from blindly cloning a whole-disk image. The installer owns the target disk layout, formats persistent filesystems on the real block device, and only writes the immutable root artifact into a bounded root slot.

## 8.3 Root slots and boot entries

Katl should initially target EFI-only systems and use systemd-boot.

Recommended boot model:

```text
ESP:
  systemd-boot
  loader configuration

XBOOTLDR or ESP:
  versioned UKIs in /EFI/Linux/
  loader entries if Type #1 entries are used

root-a/root-b:
  immutable root filesystem slots

var:
  persistent state, logs, kubelet data, containerd data, Katl update state
```

Each installed base OS version should have explicit boot metadata:

```text
katl version
root slot
root partition UUID
expected root digest
extension set
kernel command line
boot attempt policy
```

Updates should install the new base system into the inactive root slot, create a boot entry or UKI for that generation, set it as the next boot candidate, and reboot. If the new generation reaches the health target, it is marked good. If it fails boot counting or health checks, the machine should fall back to the previous known-good entry.

## 8.4 Disk geometry handling

The installer should create the partition table and filesystems on the target disk at install time.

This means:

* Do not write a whole-disk raw image directly to the device.
* Do not assume the target drive has the same logical sector size as the build host.
* Do not assume a specific physical sector size.
* Align partitions using normal GPT/tooling alignment.
* Let filesystem creation tools see the actual target block device.
* Let the installer validate the final layout using `lsblk`, `blkid`, and `findmnt`.

This avoids common failure modes with different device geometry.

## 8.5 systemd-repart role

`systemd-repart` can be used in two places:

1. During image build, to construct image artifacts.
2. During install, to apply partition definitions to the target disk.

For this project, the preferred model is:

```text
Config compiler emits repart.d-style partition definitions.
Installer runs systemd-repart or equivalent partitioning logic on the real target disk.
Installer formats and mounts the resulting partitions.
Installer installs runtime contents into the mounted target.
```

If `systemd-repart` does not fit a specific advanced case, the installer can fall back to explicit partitioning commands while preserving the same manifest model.

## 9. Immutability model

## 9.1 Runtime immutability

Once booted into the installed runtime:

```text
/usr is immutable base OS plus sysext overlays.
/etc is provided by confext overlays.
/var is writable persistent state.
```

The default runtime should not allow arbitrary persistent mutation of the OS drive.

Recommended model:

```text
root filesystem: read-only
/usr: read-only
/etc: confext overlay
/var: writable
/run: tmpfs
/tmp: tmpfs or controlled writable
```

## 9.2 Persistent writable state

The read-only root must still present stable writable paths for Kubernetes and node state.

Recommended persistent state model:

```text
/var:
  persistent writable state partition

/var/lib/katl:
  Katl node state, active generation pointers, update staging, install records

/var/lib/kubelet:
  kubelet state

/var/lib/containerd:
  container runtime state

/var/lib/etcd:
  optional separate partition or bind mount for control-plane nodes

/var/lib/katl/binds/etc-kubernetes:
  persistent kubeadm-owned /etc/kubernetes state
```

The runtime should use systemd mount units, bind mounts, tmpfiles, and explicit service ordering to project persistent state into the paths expected by kubeadm and kubelet. `/run` may contain generated ephemeral views, sockets, and readiness state, but it must not be the source of persistent Kubernetes identity because `/run` is tmpfs.

Important writable paths:

```text
/etc/kubernetes/pki
/etc/kubernetes/*.conf
/var/lib/kubelet
/var/lib/containerd
/var/lib/etcd
/var/log or journal storage
```

These paths should be owned by generated mount units or tmpfiles rules rather than ad-hoc service scripts. systemd slices can be used to isolate Katl agent and update workloads, but filesystem stability should come from mount units and explicit dependencies.

## 9.3 Is mutable recovery useful?

Yes, but only if carefully scoped.

Useful recovery modes:

1. **Ephemeral mutable recovery**

   * Boot with recovery flag.
   * Allow temporary mutation of `/etc` or root overlay.
   * Changes disappear on reboot unless explicitly exported.
   * Useful for debugging bad config.

2. **Persistent repair mode**

   * Explicitly requested.
   * Mounts state partitions writable.
   * Allows replacing confext/sysext artifacts, clearing failed update state, or changing boot target.
   * Should be noisy and auditable.

Avoid a vague “mutable OS” mode where users accidentally drift from generated config.

Recommended recovery behavior:

```text
normal boot:
  immutable runtime

recovery boot:
  read-only OS
  writable temporary overlay
  SSH enabled
  node agent disabled or in safe mode

repair command:
  explicitly modifies /var/lib/katl state, extension pointers, or boot entries
```

## 10. SSH access

SSH should be supported because target users are home-lab operators and this is not trying to be Talos.

SSH should exist in:

```text
installer
runtime, optional but recommended
recovery
```

## 10.1 Installer SSH

Installer SSH is used for:

```text
manual debugging
disk inspection
network debugging
install log collection
manual recovery
```

Installer SSH should use keys from:

```text
matchbox-rendered Ignition
ISO-embedded config
kernel cmdline emergency key URL, optional
```

Password login should be disabled by default.

## 10.2 Runtime SSH

Runtime SSH should be configurable.

Recommended default:

```text
SSH enabled for admin key-based access.
root login disabled or forced-command/recovery-only depending on project policy.
admin user has limited sudo or full sudo depending on profile.
```

Runtime SSH is useful because this project is intentionally not Talos. It should embrace standard Linux recovery while keeping generated config as the source of truth.

## 11. PXE path with matchbox

## 11.1 Matchbox responsibilities

Matchbox should provide:

```text
machine matching by MAC/UUID
iPXE profile rendering
kernel/initrd/UKI boot references
Ignition config serving
install manifest serving
per-node templating
```

The compiler should generate matchbox groups and profiles.

Example matchbox group intent:

```yaml
id: ms01-01
selector:
  mac: "00:11:22:33:44:55"
profile: katl-installer
metadata:
  node_name: ms01-01
  ignition_id: ms01-01
  install_manifest: ms01-01.install.yaml
```

## 11.2 iPXE profile

The iPXE profile should be thin.

Example:

```ipxe
#!ipxe

set base-url http://matchbox.example.net/assets
set ignition-url http://matchbox.example.net/ignition?uuid=${uuid}
set manifest-url http://matchbox.example.net/assets/manifests/${mac}.install.yaml

kernel ${base-url}/katl-installer.kernel \
  ignition.config.url=${ignition-url} \
  katl.install.manifest=${manifest-url} \
  console=tty0

initrd ${base-url}/katl-installer.initrd

boot
```

The iPXE script should not contain complex install logic. It should only boot the installer with pointers to config.

## 11.3 PXE install flow

```text
1. Node PXE boots.
2. DHCP points node to iPXE.
3. iPXE chainloads matchbox profile.
4. matchbox identifies node by MAC/UUID.
5. matchbox serves installer kernel/initrd and Ignition URL.
6. Installer boots.
7. Installer fetches install manifest.
8. Installer applies disk layout.
9. Installer installs runtime OS.
10. Installer writes first-boot Ignition seed or config.
11. Installer preloads node confext/sysext if configured.
12. Installer reboots.
13. Runtime boots from disk.
14. Ignition applies node config.
15. systemd activates sysext/confext.
16. Node reaches kubeadm-ready target.
```

## 12. USB/ISO path

## 12.1 ISO responsibilities

The ISO should support installs without matchbox.

For ISO installs, all node configs should be built into the image.

The generated ISO contains:

```text
installer kernel/initrd/UKI
installer rootfs
all node install manifests
all node Ignition configs
runtime image/assets
sysext images
confext images
node selection metadata
SSH authorized keys
```

## 12.2 Node selection

For ISO install, the installer can choose node config by:

```text
MAC address match
system UUID match
DMI serial match
interactive selection
kernel cmdline katl.node=ms01-01
```

Recommended priority:

```text
1. explicit kernel cmdline katl.node
2. hardware match
3. interactive selection
4. fail closed
```

Do not silently pick the first node.

## 12.3 ISO install flow

```text
1. User boots ISO from USB.
2. Installer starts.
3. Installer finds embedded node config bundle.
4. Installer identifies node by MAC/UUID/DMI or asks user.
5. Installer applies selected install manifest.
6. Installer installs runtime from embedded artifacts.
7. Installer writes Ignition/first-boot config.
8. Installer reboots.
9. Runtime reaches kubeadm-ready target.
```

## 13. Ignition role

Ignition is used for first-boot node configuration.

Ignition should write or enable:

```text
hostname
machine-specific files
SSH authorized keys
networkd files, if not delivered by confext
node seed file
extension activation config
systemd unit enablement
trust policy
installer completion marker
```

Ignition should not become a long-term configuration management system.

Long-term config should come from generated confext images.

A useful split:

```text
Ignition:
  identity and bootstrap seed

confext:
  steady-state /etc
```

## 14. Runtime boot model

Runtime boot ordering:

```text
local-fs.target
systemd-udevd.service
katl-state.mount and bind mounts
systemd-sysext.service
systemd-confext.service
systemd-networkd.service
network-online.target
systemd-time-wait-sync.service
systemd-sysctl.service
containerd.service
kubelet.service
katl-kubeadm-ready.target
```

The node should reach:

```text
katl-kubeadm-ready.target
```

when:

```text
host networking is configured
time is valid
sysext activation succeeded
confext activation succeeded
containerd is running
kubelet binary exists
kubeadm binary exists
kubelet service exists
kubeadm config exists
SSH access is configured as requested
```

This target is the handoff point to kubeadm.

The exact unit graph should be tested in QEMU because confext-provided `/etc` files must be visible before services such as networkd, resolved, timesyncd, containerd, and kubelet load their configuration. If confext supplies unit files or drop-ins, Katl must ensure systemd reloads units before dependent services start.

## 15. kubeadm boundary

The project should support three kubeadm modes:

```text
manual
auto-init
auto-join
```

## 15.1 manual mode

The node reaches `katl-kubeadm-ready.target`.

The user runs:

```bash
kubeadm init --config /etc/kubernetes/kubeadm-init.yaml
```

or:

```bash
kubeadm join --config /etc/kubernetes/kubeadm-join.yaml
```

This is the safest initial development mode.

## 15.2 auto-init mode

The first node automatically runs kubeadm init after the ready target.

```text
katl-kubeadm-init.service
  After=katl-kubeadm-ready.target
  ConditionPathExists=!/etc/kubernetes/admin.conf
```

## 15.3 auto-join mode

Additional nodes automatically join once join material is available.

Join material can come from:

```text
user-provided encrypted config
short-lived bootstrap endpoint
manual copy
future controller
```

For v1, prefer manual and auto-init. Add auto-join once bootstrap material handling is designed.

## 16. Build system

## 16.1 End-user asset factory

The north-star user workflow is a Git repository containing Katl configuration and a GitHub Actions workflow that produces usable installer and update assets.

Example workflow intent:

```text
1. User edits cluster config in Git.
2. GitHub Actions runs katlc validate.
3. katlc renders build inputs, install manifests, Ignition, matchbox resources, and update metadata.
4. mkosi builds installer, runtime root slots, sysexts, confexts, and ISO assets.
5. katlc signs artifacts and emits checksums, provenance, and release metadata.
6. Actions publishes assets to GitHub Releases, an OCI registry, object storage, or a matchbox asset directory.
7. New installs PXE boot or ISO boot those assets.
8. Existing nodes fetch signed update metadata and stage the next generation.
```

This means Katl must keep build outputs deterministic, scriptable, and easy to publish from CI. The local developer workflow and the CI workflow should use the same `katlc` commands.

The CLI should make the CI path obvious:

```text
katlc validate
katlc render
katlc build
katlc sign
katlc publish-plan
```

`katlc publish-plan` should be able to emit enough metadata for a GitHub Actions workflow to upload release assets without embedding project-specific release logic in the workflow itself.

## 16.2 mkosi image graph

The project should use mkosi to build multiple related images:

```text
installer
runtime
sysext-kubernetes
sysext-debug
sysext-storage
sysext-network
confext-common
confext-role-control-plane
confext-node-ms01-01
iso
```

A conceptual layout:

```text
mkosi.images/
  installer/
    mkosi.conf
    mkosi.extra/
  runtime/
    mkosi.conf
    mkosi.extra/
  sysext-kubernetes/
    mkosi.conf
    mkosi.extra/
  confext-common/
    mkosi.conf
    mkosi.extra/
  confext-node/
    mkosi.conf
    mkosi.extra/
```

## 16.3 Build outputs

The build should emit:

```text
dist/
  installer/
    katl-installer.uki
    # v0 target: one bootable installer UKI containing the installer initrd
    # and katlos-install. ISO/USB wrappers can come later.

  runtime/
    katl-runtime-v2026.05.31.squashfs
    katl-runtime-v2026.05.31.squashfs.digest
    katl-runtime-v2026.05.31.uki

  sysext/
    kubernetes-v1.34.2.raw
    debug-tools.raw

  confext/
    common.raw
    control-plane.raw
    ms01-01.raw

  ignition/
    ms01-01.ign
    ms01-02.ign

  matchbox/
    groups/
    profiles/
    assets/

  iso/
    katl-home.iso

  manifests/
    ms01-01.install.yaml
```

## 16.4 Asset signing

Every built artifact should have:

```text
digest
signature
optional SBOM
optional provenance
```

Installer and runtime should verify artifacts before use.

## 17. Asset compiler

The asset compiler transforms user config into concrete build inputs.

Inputs:

```text
cluster.yaml
node overlays
native systemd files
native Ignition snippets
disk layout definitions
SSH keys
kubeadm config templates
```

Outputs:

```text
mkosi inputs
Ignition configs
confext rootfs trees
sysext rootfs trees
install manifests
matchbox resources
ISO embedded config bundle
```

## 17.1 Rendering policy

The compiler should be conservative.

It should:

```text
validate YAML structure
validate duplicate node names
validate duplicate MAC addresses
validate duplicate static IPs when using convenience schema
validate install disk is specified
validate required SSH keys if SSH enabled
validate generated systemd units with systemd-analyze verify where possible
validate networkd files where possible
validate kubeadm config exists for kubeadm modes
```

It should not:

```text
try to understand every systemd-networkd feature
invent a new networking model
hide native systemd behavior
silently rewrite unknown native config
```

## 18. Update model

Normal updates are runtime responsibilities, not installer responsibilities.

Update categories:

```text
runtime base update
sysext update
confext update
kubeadm/Kubernetes version update
```

For v1, keep update mechanics simple:

```text
build new artifacts
publish signed artifacts
node downloads and verifies
write new base root artifact to inactive root slot
stage new sysext/confext
create or update systemd-boot entry
set next boot candidate
reboot to apply disruptive changes
health-check
mark boot successful
rollback boot entry and extension pointers on failure
```

Runtime base updates should use the same A/B root slot model as installs. The first implementation can be Katl-managed while preserving compatibility with `systemd-sysupdate` concepts where they fit.

## 18.1 Live-node update flow

Updates are applied to a live node by staging artifacts, not by mutating the active root.

Recommended flow:

```text
1. katl-agent fetches signed update metadata.
2. katl-agent verifies signatures, digests, version policy, and compatibility.
3. katl-agent downloads the runtime root artifact, sysexts, and confexts.
4. katl-agent writes the runtime root artifact to the inactive root slot.
5. katl-agent stages sysext/confext artifacts under /var/lib/katl.
6. katl-agent writes boot metadata for the new generation.
7. katl-agent uses systemd-boot boot counting or a oneshot boot entry.
8. Node reboots into the new generation.
9. katl-health gates boot-complete.target.
10. systemd-bless-boot marks the generation successful.
11. On failure, systemd-boot falls back to the previous known-good generation.
```

The active generation should be observable with a simple status command:

```text
katlctl status
katlctl update check
katlctl update stage
katlctl update reboot
katlctl rollback
```

## 19. Recovery model

Recovery should have three modes:

## 19.1 Installer recovery

Boot installer via PXE/ISO and choose recovery shell.

Capabilities:

```text
inspect disks
mount state partitions
view logs
replace extension artifacts
clear failed update state
reinstall runtime
collect diagnostics
```

## 19.2 Runtime recovery boot

Boot installed runtime with:

```text
katl.recovery=1
```

Behavior:

```text
do not run kubeadm automation
do not apply pending updates
enable SSH
mount temporary writable overlay if needed
start minimal network
start diagnostic target
```

## 19.3 Explicit repair

Persistent changes require explicit commands:

```text
katlctl repair set-active-confext previous
katlctl repair set-active-sysext previous
katlctl repair clear-pending-update
katlctl repair reinstall-boot-entry
```

This prevents accidental drift.

## 20. Security model

Security should be practical rather than theatrical.

## 20.1 Trust roots

The installer and runtime need a configured trust root for:

```text
install manifests
Ignition configs
runtime image artifacts
sysext images
confext images
matchbox-served assets
```

## 20.2 SSH

Default:

```text
key-only auth
password auth disabled
root login disabled unless recovery profile explicitly allows it
```

## 20.3 Secrets

Avoid placing long-lived secrets directly in generated assets.

For v1:

```text
SSH public keys are fine.
kubeadm bootstrap tokens should be short-lived or manually supplied.
Cluster admin kubeconfig should not be baked into ISO unless explicitly requested.
```

Later:

```text
SOPS/age encrypted node secrets
TPM-sealed node identity
Secure Boot
measured boot
dm-verity for runtime root
signed UKIs
```

## 21. Testing model

Katl needs VM-based testing from the beginning because the core product is an installable OS generator.

Test layers:

```text
unit tests:
  config parsing
  schema validation
  install plan generation
  update plan generation
  boot entry generation

golden tests:
  rendered Ignition
  rendered repart definitions
  rendered systemd units
  rendered matchbox groups/profiles
  rendered GitHub Actions publish metadata

static verification:
  systemd-analyze verify for generated units
  Ignition validation
  kubeadm config validation where possible

QEMU/libvirt integration:
  installer boots with OVMF/EFI
  installer partitions an empty disk
  installer writes root-a and persistent state
  installed runtime boots with systemd-boot
  node reaches katl-kubeadm-ready.target
  update writes root-b and reboots into it
  failed update rolls back to root-a
  ISO install works without network
  PXE/matchbox install works with served assets
```

The GitHub Actions path for end users should not require privileged virtualization. Hosted runners can run `katlc validate`, rendering tests, artifact signing, and build orchestration. Full QEMU/libvirt install tests are primarily for Katl's own CI or users with self-hosted runners.

## 22. Open questions

1. Should containerd be in the runtime base or in a sysext?

   * Initial recommendation: runtime base.
   * Later option: sysext once bootstrap is stable.

2. Should the runtime root use SquashFS, EROFS, dm-verity ext4, or another read-only format?

   * v0 decision: SquashFS root slots because the mental model is simple and the artifact is naturally read-only.
   * The SquashFS image is built before install time and written directly into the selected root slot partition.
   * Later option: EROFS or dm-verity once the base install/update path is stable.

3. Should kubeadm auto-init be a v1 feature?

   * Initial recommendation: yes for single-node bootstrap, manual for multi-node join.

4. Should matchbox be required?

   * No. It is the golden PXE path, but ISO must be first-class.

5. Should all `/etc` be confext?

   * Goal: yes for generated steady-state config.
   * Exception: kubeadm-generated certs/kubeconfigs and machine identity need carefully managed writable state.

6. Should runtime SSH be enabled by default?

   * Recommendation: yes for this target audience, key-only, generated config, auditable.

## 23. Milestones

## Milestone 1: mkosi runtime boots

Deliverables:

```text
minimal runtime image
systemd boot
networkd config
SSH access
read-only root
writable /var
```

## Milestone 2: installer installs runtime

Deliverables:

```text
installer image
install manifest
disk partitioning
filesystem creation
runtime installation
bootloader/UKI setup
reboot into runtime
```

## Milestone 3: PXE via matchbox

Deliverables:

```text
generated matchbox profiles
generated matchbox groups
generated iPXE templates
generated Ignition config
node-specific install via MAC match
```

## Milestone 4: ISO installer

Deliverables:

```text
ISO image with embedded node configs
node selection by MAC/UUID/cmdline
offline install from embedded assets
```

## Milestone 5: sysext/confext

Deliverables:

```text
Kubernetes sysext
common confext
node confext
systemd-sysext activation
systemd-confext activation
```

## Milestone 6: kubeadm-ready target

Deliverables:

```text
containerd running
kubelet installed
kubeadm installed
kubeadm config present
katl-kubeadm-ready.target
manual kubeadm init succeeds
```

## Milestone 7: recovery and repair

Deliverables:

```text
installer recovery shell
runtime recovery boot
SSH recovery access
katlctl repair commands
```

## Milestone 8: update groundwork

Deliverables:

```text
signed artifacts
extension staging
extension rollback
runtime status reporting
```

## 24. Final product definition

This project is:

```text
A systemd-native Kubernetes Cluster OS builder.
```

It lets users define a small cluster in Git and compile it into installable assets. Nodes are installed through matchbox/iPXE or USB/ISO. The installer applies a real disk layout, writes node configuration, installs an immutable runtime OS, and reboots. The runtime uses systemd, sysext, confext, and Ignition-derived identity to reach a deterministic kubeadm-ready state. From there, kubeadm and Kubernetes-native GitOps take over.

It is not a general-purpose OS, not a Kubernetes distribution, and not a Talos clone. It is a Kubernetes node OS generator built from standard Linux and systemd mechanisms.
