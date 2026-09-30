# KatlOS support boundary

KatlOS is stable software for home-lab use and development, beginning with
2026.9.0. Stable means the maintainer considers Katl ready to use and is
committed to continued use and fixing issues in subsequent releases. It does
not mean that future releases will be free of bugs, regressions, or breaking
changes.

It is not supported for production clusters, security-sensitive workloads,
compliance environments, or systems whose availability depends on KatlOS.
There is no support SLA or security-response SLA. The network management API
has a bounded compatibility guarantee beginning with the first stable release;
see [agent API compatibility](internal/agent-api-compatibility.md).

## Supported home-lab surface

The supported release surface is deliberately narrow:

- x86-64 machines booted with UEFI;
- the self-contained installer ISO, or the matching loose UEFI/PXE artifacts;
- one `config.katl.dev/v1alpha1` `ClusterConfig` compiled by the matching
  `katlctl` release;
- one explicitly selected target disk per node, with destructive wipe consent;
- kubeadm bootstrap using the published Kubernetes bundle named in the install
  guide; and
- node-local runtime configuration for the supported domains described in
  [Apply cluster configuration](operations/configure-nodes.md).

Release claims extend only to the exact VM and physical-hardware paths named in
that release's retained evidence. The automated capable-host path uses libvirt,
KVM, and OVMF. A successful VM run is not a general hardware compatibility
claim. Firmware, storage controllers, network devices, and physical machines
not named in release evidence are unverified.

Katl prepares kubeadm-ready nodes and performs bounded bootstrap operations. It
does not provide a Kubernetes distribution. Users own DHCP/PXE infrastructure,
DNS, CNI, GitOps, storage, ingress, workload policy, monitoring, backup, and
application lifecycle.

KatlOS standard follows Fedora's stable kernel packages. KatlOS-lts uses the
maintained kwizart 6.18 LTS kernel RPMs for the same Fedora release. Both have
the same support boundary; LTS does not extend Fedora userspace support.
See [kernel flavours](operations/upgrade-host.md#kernel-flavours) for selection
and upgrade behavior.

## Artifact trust

KatlOS release assets provide SHA-256 checksums and keyless GitHub build-
provenance attestations. Kubernetes bundles are digest-addressable OCI
artifacts with GitHub provenance. These are optional tools for operators who
want a stricter supply-chain policy; the normal home-lab path accepts readable
release and bundle versions and performs its own internal consistency checks.

The `katlc` management API on TCP `9443` defaults to trusted-network access for
new configurations. Anyone who can reach that API can manage the node; the
connection is not encrypted. Opt-in mTLS authenticates and encrypts management
connections. Keep nodes on a trusted management LAN. See
[management access](operations/access.md) for mode selection and existing-node
upgrade behavior.

The ISO install handoff is intentionally unauthenticated HTTP for the supported
trusted home-lab path. Restrict port 8080 to the provisioning network: the
installer accepts one structurally valid configuration and then closes the
handoff path.

This proves which repository workflow produced the bytes. It does not provide:

- UEFI Secure Boot signatures or a production boot-key policy;
- node-side signature-policy enforcement;
- artifact revocation, downgrade prevention, or a vulnerability-free claim;
- confidential secret distribution; or
- a production supply-chain or incident-response guarantee.

## Compatibility promise

The stable release designation does not freeze every interface or format.
The `v1alpha1` configuration authoring format may still change incompatibly;
use the matching release's CLI to validate and compile it. Katl does not
promise automatic state migration or an upgrade path from every development
or beta build. Beginning with 2026.9.0, the node's network management
API follows the [agent API compatibility policy](internal/agent-api-compatibility.md):
a newer `katlctl` supports the current and two preceding stable release series
where the node has the required capability. Preserve the source `ClusterConfig`,
exact release assets, checksums, OCI digests, and recovery data. Reinstall may
be required after an incompatible change outside that API guarantee.

For stable host upgrades, matching `runtimeInterface` values also declare that
the target and the retained source can use the same Katl records and writable
service state across that window. Katl rejects a mismatched runtime interface
before target preparation or inactive-slot writes. Follow the target release's
migration or reinstall procedure instead of bypassing this check.

Use the `katlctl` binary from the same KatlOS release to validate and compile
configuration. The management API window does not make configuration formats
from different release trains interchangeable.

## Upgrade and recovery limits

KatlOS host update and rollback are node-local root, UKI, sysext, and confext
operations. They do not roll back etcd, kubeadm mutations, Kubernetes API
objects, persistent volumes, application data, or external infrastructure.
After a partial kubeadm or Kubernetes mutation, the node may report that manual
recovery is required.

KatlOS has two root slots, so only the active OS and one peer OS can remain
bootable. A later upgrade overwrites the peer slot; extra generation records do
not retain extra runtime roots. Both slots also share `/var`, the EFI System
Partition, firmware boot state, and usually one disk. A/B rollback does not
recover failure or corruption in those shared components.

Kubernetes upgrades support an explicit serial rollout to a newer patch or the
next minor using a published Katl bundle. A healthy multi-control-plane cluster
supports replacing one control plane at a time, including explicit stale etcd
member removal when quorum remains. This is not etcd disaster recovery,
automatic post-mutation repair, or general cluster reconciliation.
Wipe/reinstall is destructive recovery, not backup. Keep independent etcd,
workload, and data backups; do not rely on Katl generation rollback as a
cluster backup.

## Explicitly unsupported

Do not use KatlOS as the basis for:

- production, regulated, multi-tenant, or security-critical clusters;
- an availability or disaster-recovery commitment;
- unattended host or Kubernetes fleet upgrades;
- Secure Boot or measured-boot policy enforcement;
- hardware enablement beyond retained release evidence;
- API, schema, or on-disk-state compatibility beyond the documented window,
  or long-term Kubernetes support promises; or
- private artifact and credential distribution policy.

## Report a problem

Open a [Katl GitHub issue](https://github.com/katl-dev/katl/issues/new/choose)
using the bug report form. Remove tokens, private keys, kubeconfigs, join
commands, and other secrets before attaching anything. Include:

- the KatlOS version, release URL, and `katlctl version` output;
- exact artifact filenames, configured Kubernetes version, and any resolved
  bundle identity reported in operation evidence, plus SHA-256 values or
  provenance results when they are relevant and available;
- hardware or hypervisor, firmware/UEFI mode, CPU, storage controller and disk
  identity, and network devices;
- the smallest redacted `ClusterConfig` and exact command sequence that
  reproduces the problem;
- relevant operation IDs, generation IDs, selected node, and failure
  timestamps; and
- redacted installer output, `systemctl` status, `journalctl` output, or retained
  `scripts/vmtest-run` run directory named by the failure.

A report is evidence for investigation, not a support entitlement. Security
reports that should not be public must use the repository's
[private vulnerability reporting](https://github.com/katl-dev/katl/security/advisories/new)
rather than a public issue.
