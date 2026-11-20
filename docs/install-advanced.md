# Advanced installer networking

Use static IPv4 settings when the installer boots on a network without DHCP
or needs a predetermined address. With the release ISO, attach a seed medium
that contains native systemd-networkd files. For network boot, pass the
settings as kernel arguments in each machine's iPXE or Matchbox profile.
Use installer artifacts and `katlctl` from the same release.
[Installation prerequisites](installing.md#prerequisites)

The installer can use a temporary address for handoff. The node's
`management.address` and networkd files in `ClusterConfig` govern how you
reach it after it boots from disk. You can use the same address for both
stages or plan a change at reboot.

## Plan the addresses and routes

Before booting, record the MAC address of each installer's Ethernet port, its
static address and prefix, and any gateway and DNS servers it needs. Use a
unique address for each machine. For the installed node, record its management
address and how the workstation will reach it.

The examples use `cp-1` with MAC `52:54:00:aa:bb:15`, installer address
`192.0.2.20/24`, and gateway `192.0.2.1`. Its installed address will be
`192.0.2.30/24`. The workstation is on a different subnet at
`198.51.100.10`, and the provisioning server and DNS server are at
`198.51.100.5` and `198.51.100.53` respectively. Replace these values with
your own.

For routed installs, check that the workstation can route to both node
addresses and that the live installer and installed node have a return route.
Allow the workstation to reach the live installer on TCP `8080` for handoff
and the installed node on TCP `9443` for management. Keep installer handoff on
trusted networks because the endpoint accepts unauthenticated requests. For
network boot, also allow the machine to reach its provisioning server and
artifact URLs. If those URLs use names, supply a reachable DNS server.

## Boot the ISO with a seed medium

Save this file as `10-cp-1.network`. The MAC match selects one Ethernet port.
Networkd applies the first matching file in filename order, so the `10-`
profile takes precedence over the installer's `80-` DHCP fallback on that port.

```ini
[Match]
MACAddress=52:54:00:aa:bb:15

[Network]
DHCP=no
Address=192.0.2.20/24
Gateway=192.0.2.1
DNS=198.51.100.53
```

For physical media, use a separate USB drive with a filesystem labeled
`KATLSEED`. Copy the file to `etc/systemd/network/10-cp-1.network` on that
filesystem. For virtual media, you can create a small FAT disk image with
`mkfs.vfat` and mtools. Run these commands from the directory containing
`10-cp-1.network`, then attach the printed image path as a raw virtual disk
from a storage path your hypervisor can read.

```sh
seed_dir=$(mktemp -d)
seed_image="$seed_dir/katl-seed.img"
truncate -s 16M "$seed_image"
mkfs.vfat -n KATLSEED "$seed_image"
mmd -i "$seed_image" ::/etc ::/etc/systemd ::/etc/systemd/network
mcopy -i "$seed_image" ./10-cp-1.network ::/etc/systemd/network/
printf '%s\n' "$seed_image"
```

To reuse the seed medium on multiple machines, add one file per machine with
a distinct filename, MAC, and address. For example, add
`10-worker-1.network` with MAC `52:54:00:aa:bb:16` and address
`192.0.2.21/24` for a second node on the same subnet.

Attach the seed medium and boot the release ISO in UEFI mode. On the installer
console, check that the expected static address appears. Then confirm handoff
from the workstation.

```sh
katlctl install status --endpoint 192.0.2.20
```

The status should report `waiting-for-config`. Other Ethernet ports may still
use DHCP. Use the static address you configured when targeting this machine.
Continue with [install using the live address](#install-using-the-live-address).

## Pass static settings during network boot

The examples use an installer with systemd 261 or later, which supports
selecting a network interface by the `BOOTIF` placeholder in `ip=`.
`BOOTIF=01-52-54-00-aa-bb-15` identifies the Ethernet MAC
`52:54:00:aa:bb:15`. In the `ip=` argument, the fields set the address, gateway,
IPv4 netmask, interface, and configuration mode. The empty fields omit a peer
address and hostname. The `none` mode selects static networking on that interface.
[systemd network generator](https://www.freedesktop.org/software/systemd/man/262/systemd-network-generator.service.html)

The kernel arguments take effect after iPXE has downloaded the kernel and
initrd. Set up iPXE's route to the boot server first. If firmware or iPXE
uses a DHCP server on another subnet, configure your site's DHCP relay.
The Linux installer then uses the settings below to fetch the bundle and
install image.

### Matchbox

Use the [per-machine Matchbox profile](install-pxe-matchbox.md#lay-out-matchbox-data).
Replace its `"ip=dhcp"` entry with these three entries in the `boot.args` array.
Use the same MAC in the Matchbox group selector and the `BOOTIF` argument.

```json
[
  "BOOTIF=01-52-54-00-aa-bb-15",
  "ip=192.0.2.20::192.0.2.1:255.255.255.0::BOOTIF:none",
  "nameserver=198.51.100.53"
]
```

For each machine, set its own MAC and address. Keep the selected node and
bundle URL from that machine's profile. The same bundle can contain the
installed network settings for all nodes.

### iPXE

If you boot iPXE locally on a network without DHCP, configure its interface
before fetching a remote script. For example, enter the following in the
iPXE shell or include them in a locally embedded script. Use the interface
connected to your provisioning network in place of `net0`.
[iPXE static settings](https://ipxe.org/settings)

```ipxe
set net0/ip 192.0.2.20
set net0/netmask 255.255.255.0
set net0/gateway 192.0.2.1
set dns 198.51.100.53
ifopen net0
```

Use this per-machine script if you serve boot files directly. Replace the base
URL with the directory containing the release kernel, initrd, and compiled
`cluster.katlcfg`. Publish the bundle and its referenced image first. This
example requests automatic installation, which can erase the disk selected
for `cp-1`. Verify the disk selector before booting the machine.

```ipxe
#!ipxe
set base http://198.51.100.5/katl
set node cp-1
set bootif 01-52-54-00-aa-bb-15
set static-ip 192.0.2.20::192.0.2.1:255.255.255.0::BOOTIF:none
kernel ${base}/katl-installer.vmlinuz initrd=katl-installer.initrd rd.systemd.unit=katl-installer.target rd.neednet=1 BOOTIF=${bootif} ip=${static-ip} nameserver=198.51.100.53 katl.node=${node} katl.bundle.url=${base}/cluster.katlcfg katl.install.mode=auto
initrd ${base}/katl-installer.initrd
boot
```

For automatic installation with either boot method,
[configure the installed address and networkd profile](#configure-the-installed-network) before
[compiling the bundle](install-pxe-matchbox.md#compile-one-cluster-configuration-bundle).
After the node reboots from disk, [check its management address](#verify-the-installed-node).
If the installer stays available after refusing a disk, follow
[the retry procedure](install-pxe-matchbox.md#retry-a-refused-installation).

## Configure the installed network

Use the [networkd profile](#boot-the-iso-with-a-seed-medium) as a starting point
for the installed node. Run the following commands beside `cluster.yaml`.
In this example the node keeps the same NIC, prefix, gateway, and DNS server,
and changes to `192.0.2.30`.

```sh
mkdir -p ./host-files
sed 's|^Address=192\.0\.2\.20/24$|Address=192.0.2.30/24|' \
  ./10-cp-1.network > ./host-files/10-cp-1.network
```

Merge these fields into the existing `cp-1` entry in `spec.nodes`. Keep the
node's role and reviewed disk selection.

```yaml
management:
  address: 192.0.2.30
hostConfiguration:
  fileSets:
    management-network:
      files:
        - path: /etc/systemd/network/10-cp-1.network
          source: host-files/10-cp-1.network
```

To keep the same address after installation, use the live address for
`management.address` and copy the networkd file with its address intact.
If the management network changes, update the installed profile and check
that the workstation can route to that network before installing.

## Install using the live address

For an ISO installer waiting for handoff, create a cluster configuration from
its live address. If the machine has multiple selectable disks, see
[how to select the target disk](installing.md#select-the-target-disk).

```sh
katlctl config init ./cluster.yaml --installer 192.0.2.20
```

Use the generated configuration or your existing configuration for this node.
[Configure the installed network](#configure-the-installed-network) before
submitting it to the installer.

Validate the configuration and review the selected disk. The following apply
command erases that disk. Pass the live IP to `--endpoint` because
`management.address` already points to the installed node's future address.

```sh
katlctl config validate ./cluster.yaml
katlctl install apply --config ./cluster.yaml --node cp-1 --endpoint 192.0.2.20
```

## Verify the installed node

After the node reboots from disk, query it using the same `ClusterConfig`.
The command uses the installed management address, `192.0.2.30` in the
example.

```sh
katlctl node status cp-1 --config ./cluster.yaml
```

Confirm the node reaches health `OK` at generation `0`. If health is
`pending`, wait for the node to finish booting and rerun the command.
[Verify generation 0](operations/access.md#confirm-generation-0)

If the workstation can reach the installer but loses access after reboot,
check the installed address and routes from the node console. Check that the
selected networkd file matches the intended NIC and that the firewall allows
TCP `9443` to the installed address.

## Diagnose installer reachability

If you cannot reach the live installer, verify the MAC, address, prefix, and
return route in its console or recovery shell. In the example, run
`ip -4 route get 198.51.100.10` from the installer to verify its route to the
workstation. From the workstation, rerun `katlctl install status --endpoint`
against the configured IP. If no address appears when booting from the ISO,
check that the seed is attached at boot, labeled `KATLSEED`, and contains the
profile under `etc/systemd/network/`. [Collect installer evidence](operations/troubleshoot.md#installer-evidence)
