# Install KatlOS with PXE and Matchbox

Matchbox selects a machine by MAC address and boots the KatlOS release
kernel and initrd. One compiled `.katlcfg` supplies installation and Kubernetes
intent for every node. The selected node installs automatically and reaches generation 0, ready
for `katlctl cluster bootstrap`.

Katl does not run DHCP, TFTP, iPXE, or Matchbox. Those services are
operator-owned and must be isolated from networks where they are not intended
to answer. The container example below is for a dedicated lab bridge; adapt the
profile to existing provisioning infrastructure on real hardware.

## Required release assets

Keep all files from one Katl release:

```text
katl-installer.vmlinuz
katl-installer.initrd
katlos-install-<version>-x86_64.squashfs
katlos-install-<version>-x86_64.squashfs.json
katlctl-<version>-linux-amd64
```

On an Apple Silicon Mac, use `katlctl-<version>-darwin-arm64` in place of the
Linux CLI.

Verify their adjacent checksums before publishing. The kernel and initrd boot
the temporary installer. The SquashFS is the verified KatlOS payload written to
the selected disk.

## Compile one cluster configuration bundle

Author and validate your [cluster configuration](reference/cluster-config.md).
It must contain stable disk
selectors, management addresses, node roles, Kubernetes version, and SSH keys
for every node:

```sh
katlctl config validate ./cluster.yaml
```

Compile one bundle while naming the URL from which installed machines can
fetch the matching KatlOS payload:

```sh
VERSION=2026.7.0-beta.1
BASE_URL="http://192.168.254.1:8080/assets/katl/$VERSION"
katlctl config bundle ./cluster.yaml \
  --output ./cluster.katlcfg \
  --katlos-image-url "$BASE_URL/katlos-install-$VERSION-x86_64.squashfs" \
  --katlos-image-metadata "./katlos-install-$VERSION-x86_64.squashfs.json"
sha256sum ./cluster.katlcfg
```

The URL becomes part of the compiled install plan, so it must remain reachable
from the live installer. The bundle carries all node plans and the native
kubeadm inputs needed later; no separate per-node configuration file is required.

When mTLS management is configured, the bundle contains each node's
server private key. Katl writes the bundle mode `0600`. Publish it only on the
trusted provisioning network, restrict access from workload networks, and
remove the published copy after installation. Back up the file referenced by
`spec.managementIdentity` separately; the bundle cannot replace that authority.
Trusted-network management uses no management keys.

## Lay out Matchbox data

Use Matchbox's normal file store:

```text
matchbox/
├── assets/
│   └── katl/
│       └── 2026.7.0-beta.1/
│           ├── cluster.katlcfg
│           ├── katl-installer.initrd
│           ├── katl-installer.vmlinuz
│           ├── katlos-install-2026.7.0-beta.1-x86_64.squashfs
│           └── katlos-install-2026.7.0-beta.1-x86_64.squashfs.json
├── groups/
│   ├── cp-1.json
│   └── worker-1.json
└── profiles/
    ├── katl-cp-1.json
    └── katl-worker-1.json
```

Matchbox profiles are ordinary JSON. This `cp-1` profile uses one fixed node
selection and the shared bundle:

```json
{
  "id": "katl-cp-1",
  "name": "Install KatlOS cp-1",
  "boot": {
    "kernel": "/assets/katl/2026.7.0-beta.1/katl-installer.vmlinuz",
    "initrd": [
      "/assets/katl/2026.7.0-beta.1/katl-installer.initrd"
    ],
    "args": [
      "initrd=katl-installer.initrd",
      "rd.systemd.unit=katl-installer.target",
      "rd.neednet=1",
      "ip=dhcp",
      "console=tty3",
      "console=ttyS0,115200n8",
      "katl.bundle.url=http://192.168.254.1:8080/assets/katl/2026.7.0-beta.1/cluster.katlcfg",
      "katl.bundle.sha256=REPLACE_WITH_CLUSTER_KATLCFG_SHA256",
      "katl.node=cp-1",
      "katl.install.mode=auto"
    ]
  }
}
```

The group binds that profile to one normalized MAC address:

```json
{
  "name": "cp-1",
  "profile": "katl-cp-1",
  "selector": {
    "mac": "52:54:00:aa:bb:15"
  }
}
```

Create one group and profile per machine, changing only the MAC, profile ID, and
`katl.node`. Every profile can use the same bundle URL and digest. A fixed node
argument avoids depending on DHCP hostname behavior or template expansion in a
network-boot program.

`katl.bundle.sha256` is optional because Katl validates the archive and its
internal descriptors. Supplying it pins the external handoff to the reviewed
bundle bytes and catches accidental replacement before extraction.

## Retry a refused installation

If automatic installation refuses a disk or volume during planning, it keeps its
HTTP handoff available and displays the original error and a retry command.
Use the displayed installer address, which may differ from the installed node's
management address. Correct the configuration or environment, then submit the
configuration again with `katlctl install apply`.

Set `wipe: true` on a volume when its selected disk or partition should be
formatted, including when it contains existing data. This is sufficient for
automatic PXE installation; no separate wipe acknowledgement is needed. With
`wipe: false`, incompatible contents are preserved and installation refuses
before mutation. Correct the volume configuration and retry:

```sh
katlctl install apply --config ./cluster.yaml --node cp-1 \
  --endpoint http://192.168.254.100:8080
```

Failures after disk mutation do not enable this safe retry path; preserve the
diagnostics and inspect the target before recovery.

## Run Matchbox on an isolated lab bridge

Matchbox can serve the file store directly from a pinned container:

```sh
podman run --rm --name katl-matchbox --network host \
  -v "$PWD/matchbox:/var/lib/matchbox:Z,ro" \
  quay.io/poseidon/matchbox:v0.11.0 \
  -address=0.0.0.0:8080 -log-level=debug
```

Confirm the service, profile match, and asset paths before starting a VM:

```sh
curl http://192.168.254.1:8080/
curl 'http://192.168.254.1:8080/ipxe?mac=52:54:00:aa:bb:15'
curl --fail --output /dev/null \
  http://192.168.254.1:8080/assets/katl/2026.7.0-beta.1/cluster.katlcfg
```

For a dedicated libvirt bridge named `katl-pxe0`, Poseidon's dnsmasq container
can provide DHCP, TFTP, and UEFI iPXE chainloading. Do **not** run this on a
shared LAN with another DHCP server:

```sh
podman run --rm --name katl-pxe-dnsmasq \
  --network host --cap-add NET_ADMIN \
  quay.io/poseidon/dnsmasq:v0.5.0 \
  -d -q --interface=katl-pxe0 --bind-interfaces \
  --dhcp-range=192.168.254.100,192.168.254.200,255.255.255.0,1h \
  --enable-tftp --tftp-root=/var/lib/tftpboot \
  --dhcp-match=set:efi64,option:client-arch,7 \
  --dhcp-match=set:efi64,option:client-arch,9 \
  --dhcp-boot=tag:efi64,ipxe.efi \
  --dhcp-userclass=set:ipxe,iPXE \
  --dhcp-boot=tag:ipxe,http://192.168.254.1:8080/boot.ipxe \
  --log-dhcp
```

Matchbox's upstream [network setup](https://matchbox.psdn.io/network-setup/)
documents proxy-DHCP and existing-infrastructure variants. Katl assumes UEFI;
the BIOS `undionly.kpxe` path is outside Katl's supported boot boundary.

## Boot, observe, and verify installation

Boot a blank UEFI VM or machine from the selected NIC. The expected request
chain is:

```text
firmware -> DHCP/TFTP ipxe.efi -> Matchbox /boot.ipxe
         -> Katl kernel/initrd -> shared cluster.katlcfg
         -> matching KatlOS SquashFS -> selected system disk -> reboot
```

The installer waits for usable networking before fetching the bundle, installs
only after full plan and image validation, and keeps the selected node's SSH
keys available as ephemeral `root` access while it runs. Observe the serial or
VGA console and Matchbox access log. After autonomous reboot, verify through
the normal public path:

```sh
katlctl node status cp-1 --config ./cluster.yaml
ssh katl@192.168.254.110
```

Generation 0 should report healthy with Kubernetes not configured. When every
intended node reaches this state, follow
[Bootstrap Kubernetes](operations/bootstrap-kubernetes.md). The same retained
`cluster.yaml` or published `.katlcfg` drives bootstrap; the result is ready for
you to install a CNI.

## Keep PXE first safely

Automatic install profiles authorize replacement of the selected system disk,
including an existing Katl installation. Remove the install profile or switch
firmware to disk boot after installation.

If you want repeated PXE boots to refuse an existing Katl target instead, add
`katl.halt-if-installed=1`. The installer reports a fatal refusal before
mutation and retains API and SSH access. Run `katlctl install apply --config
cluster.yaml --node NODE` to explicitly reinstall from that installer, or remove
the guard for automatic reinstallation. Runtime-agent access is not required.

## Supply boot arguments from another PXE tool

Serve the same kernel, initial RAM disk, image with its descriptor, and shared
configuration bundle from your provisioning infrastructure. The installer
uses the bundle and node name to select the disk and the OS image.

| Boot argument | Effect |
| --- | --- |
| `katl.bundle.url=URL` | Fetch the bundle from the provisioning server. |
| `katl.bundle.sha256=SHA256` | Require this exact downloaded bundle checksum. |
| `katl.bundle=PATH` | Read a bundle already available to the installer. |
| `katl.node=NAME` | Select the node's entry in the bundle. |
| `katl.install.mode=auto` | Install automatically after validation. This can erase an existing system. |
| `katl.halt-if-installed=1` | Refuse automatic installation if the disk already has Katl partition labels. |
| `katl.wait-for-config=1` | Wait for a configuration through the installer handoff. |
| `katl.hold-for-debug=1` | Hold the installer for debugging without starting an install. |
| `console=...` and `ip=...` | Select console and network settings for the installer. |

When using iPXE directly, replace the base URL with the directory where you
serve files for the selected release.

```ipxe
#!ipxe
set base https://boot.example.invalid/katl/2026.7.0
set node cp-1
kernel ${base}/katl-installer.vmlinuz initrd=katl-installer.initrd console=ttyS0,115200n8 systemd.getty_auto=no katl.node=${node} katl.bundle.url=${base}/katl-lab.katlcfg katl.install.mode=auto
initrd ${base}/katl-installer.initrd
boot
```

When a bundle selects a node, Katl configures live installer SSH access for
`root` before image validation or disk planning. The installed host uses
the `katl` account and the same public keys.

## Diagnose the handoff

If the machine does not install, preserve the failed disk and console state.
Check, in order:

1. DHCP lease and UEFI iPXE chainload;
2. Matchbox group selection at `/ipxe?mac=...`;
3. HTTP access to kernel, initrd, bundle, metadata, and SquashFS;
4. exact `katl.node`, bundle digest, and stable disk selector;
5. installer journal and `/var/lib/katl/install` state over console or SSH.

A failure before validation completes must not repartition the target. Follow
the [installer evidence checklist](operations/troubleshoot.md#installer-evidence)
before changing inputs or wiping the VM.
