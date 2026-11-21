# KatlOS support boundary

Use this page to decide whether your hardware, network, and recovery
requirements fit the supported KatlOS workflows.

Since 2026.9.0, KatlOS has been stable software for home-lab use and
development: the maintainer considers Katl ready to use and is committed to
continuing to use it and to fixing issues in subsequent releases. Stability
does not promise that future releases will be free of bugs, regressions, or
breaking changes.

KatlOS is not supported for production, regulated, multi-tenant, or
security-critical clusters, security-sensitive workloads, compliance
environments, or systems whose availability depends on KatlOS. It makes no
availability or disaster-recovery commitment, and there is no support SLA or
security-response SLA.

## Supported home-lab surface

The supported release surface is deliberately narrow:

- x86-64 machines booted with UEFI.
- The self-contained installer ISO, or the matching loose UEFI/PXE artifacts.
- One `config.katl.dev/v1alpha1` ClusterConfig compiled by the matching
  `katlctl` release.
- One explicitly selected target disk per node, with destructive wipe consent.
- kubeadm bootstrap using the published Kubernetes bundle for the version that
  `katlctl config init` writes to `spec.kubernetes.version`, as used in
  [Build your first KatlOS cluster](getting-started.md).
- Node-local runtime configuration for the supported domains described in
  [Apply cluster configuration](operations/configure-nodes.md).

Each release is qualified by automated install, bootstrap, configuration,
management, and upgrade journeys in libvirt, KVM, and OVMF virtual machines for
both kernel flavours; see [release qualification](development/releases.md#qualification-and-promotion).
A release makes no claim for physical hardware unless its release notes name
that hardware, and a successful VM run does not establish general hardware
compatibility. Firmware, storage controllers, network devices, and physical
machines that a release was not tested on are unverified.

Katl prepares kubeadm-ready nodes and performs bounded bootstrap operations. It
does not provide a Kubernetes distribution. Users own DHCP/PXE infrastructure,
DNS, CNI, GitOps, storage, ingress, workload policy, monitoring, backup, and
application lifecycle.

The standard kernel flavour follows Fedora's stable kernel packages, and the
lts flavour uses the maintained kwizart 6.18 LTS kernel RPMs for the same
Fedora release. Both have the same support boundary; LTS does not extend
Fedora userspace support. See
[kernel flavours](operations/upgrade-host.md#kernel-flavours) for selection
and upgrade behavior.

## Network trust

KatlOS expects its installer and management ports to be reachable only from
networks you trust, because by default neither port requires credentials and
neither encrypts its traffic.

| Port | Served by | Default access | What network access grants |
| --- | --- | --- | --- |
| TCP `8080` | Installer (ISO or PXE live environment) | Unauthenticated plaintext HTTP | Submitting an install configuration to a waiting installer. The installer accepts one structurally valid configuration and then closes the handoff path. |
| TCP `9443` | Management API, served by the `katlc` management agent | `trusted-network`: unauthenticated plaintext | Full node management, including destructive operations. |

Restrict port `8080` to the provisioning network, and keep installed nodes on
a trusted management LAN. The installer handoff is intentionally
unauthenticated HTTP for this trusted home-lab path.

When the management connection needs authentication and encryption, choose
opt-in mTLS (`spec.managementAuthentication: mtls`) before you install.
Changing the mode requires deliberate reprovisioning, and a failed TLS
handshake never falls back to trusted-network access. Kubernetes kubeconfigs
and SSH keys are separate from this setting. See
[Secure the management API](operations/management-auth.md#choose-an-access-mode)
for mode selection, management secrets, and existing-node upgrade behavior.

KatlOS ships no host packet filter. The nftables, conntrack, and `iptables-nft`
tools are installed, but `nftables.service` is removed from the image, so no
ruleset restricts which networks reach a node. By default an installed node
listens on SSH port `22`, which accepts public keys only, and on the
management API port `9443`; control planes also serve Katl's API proxy on
`7445`. The installer environment serves port `8080`. After bootstrap, kubelet
listens on `10250` and kube-proxy reports health on `10256`; control planes
add the API server on `6443` and etcd on `2379`–`2380`, and your CNI adds its
own ports. Other Kubernetes components listen on loopback only. If your threat
model needs tighter access, restrict it in the network in front of the nodes.

## Artifact trust

KatlOS release assets come with SHA-256 checksums and keyless GitHub
build-provenance attestations, and Kubernetes bundles are digest-addressable
OCI artifacts with GitHub provenance. Provenance proves which repository
workflow produced the bytes. These are optional tools for operators who want a
stricter supply-chain policy; the normal home-lab path accepts readable release
and Kubernetes bundle versions and performs its own internal consistency
checks. To use them, see [Verify release artifacts](operations/verify-release.md).

Checksums and provenance do not provide:

- UEFI Secure Boot signatures or a production boot-key policy.
- Node-side signature-policy enforcement.
- Artifact revocation, downgrade prevention, or a vulnerability-free claim.
- Confidential secret distribution.
- A production supply-chain or incident-response guarantee.

## Compatibility promise

The stable release designation does not freeze every interface or format.

| Area | Rule |
| --- | --- |
| Configuration format | The `v1alpha1` configuration authoring format may still change incompatibly. Use the `katlctl` binary from the same KatlOS release to validate and compile configuration; the management API window does not make configuration formats from different release trains interchangeable. |
| Management API window | Beginning with 2026.9.0, a newer `katlctl` supports management operations against nodes from the current stable release series and the two preceding stable series. A series is a distinct `YYYY.M` line; patch releases and pre-releases do not advance the window. A feature can require a newer node if the installed node lacks that capability. |
| Host upgrade runtime interface | For stable host upgrades, matching `runtimeInterface` values also declare that the target and the retained source can use the same Katl records and writable service state across that window. Katl rejects a mismatched runtime interface before target preparation or inactive-slot writes. Follow the target release's migration or reinstall procedure instead of bypassing this check. |
| State migration | Katl does not promise automatic state migration or an upgrade path from every development or beta build. Reinstall may be required after an incompatible change outside the management API guarantee. |

Preserve `cluster.yaml` and the files it references, the exact release assets,
checksums, OCI digests, and recovery data, so that you can reinstall if a change
falls outside these rules.

## Upgrade and recovery limits

KatlOS host update and rollback are node-local root, UKI, sysext, and confext
operations, so a host rollback does not restore cluster or application state;
see [Rollback is not a backup](concepts/generations.md#rollback-is-not-a-backup).
After a partial kubeadm or Kubernetes mutation, the node may report that manual
recovery is required.

KatlOS has two root slots, so only the active OS and one peer OS can remain
bootable (see [retention](concepts/generations.md#retention)). Both slots also
share `/var`, the EFI System Partition, firmware boot state, and usually one
disk, and A/B rollback does not recover failure or corruption in those shared
components.

Kubernetes upgrades support an explicit serial rollout to a newer patch or the
next minor using a published Kubernetes bundle. A healthy multi-control-plane
cluster supports replacing one control plane at a time, including explicit
stale etcd member removal when quorum remains. This is not etcd disaster
recovery, automatic post-mutation repair, or general cluster reconciliation.
Wipe/reinstall is destructive recovery, not backup, so keep independent etcd,
workload, and data backups; see [Back up etcd](operations/back-up-etcd.md),
which also states the restore limitations.

## Explicitly unsupported

Beyond the uses excluded at the top of this page, do not use KatlOS as the
basis for:

- Unattended host or Kubernetes fleet upgrades.
- Secure Boot or measured-boot policy enforcement.
- Hardware enablement beyond the configurations tested for a release.
- API, schema, or on-disk-state compatibility beyond the documented window,
  or long-term Kubernetes support promises.
- Private artifact and credential distribution policy.

## Report a problem

Open a [Katl GitHub issue](https://github.com/katl-dev/katl/issues/new/choose)
using the bug report form. Remove tokens, private keys, kubeconfigs, join
commands, and other secrets before attaching anything. Include:

- The KatlOS version, release URL, and `katlctl version` output.
- The exact artifact filenames, the configured Kubernetes version, and any
  resolved Kubernetes bundle identity that Katl reported, plus SHA-256 values
  or provenance results when they are relevant and available.
- The hardware or hypervisor, firmware/UEFI mode, CPU, storage controller and
  disk identity, and network devices.
- The smallest redacted `cluster.yaml` and exact command sequence that
  reproduces the problem.
- Relevant operation IDs, generation IDs, selected node, and failure
  timestamps.
- Redacted installer output, `systemctl` status, or `journalctl` output.

If the failure came from a Katl test run while you were developing Katl, see
[Developing Katl](developing.md) for the test output to attach.

A report is evidence for investigation, not a support entitlement. Security
reports that should not be public must use the repository's
[private vulnerability reporting](https://github.com/katl-dev/katl/security/advisories/new)
rather than a public issue.
