# KatlOS Support Boundary

KatlOS is experimental alpha software for evaluation and development. It is not
supported for production clusters, security-sensitive workloads, compliance
environments, or systems whose availability depends on KatlOS. There is no
support SLA, security-response SLA, or compatibility guarantee.

## Supported Evaluation Surface

The first alpha release surface is deliberately narrow:

- x86-64 machines booted with UEFI;
- the self-contained installer ISO, or the matching loose UEFI/PXE artifacts;
- one `config.katl.dev/v1alpha1` `ClusterConfig` compiled by the matching
  `katlctl` release;
- one explicitly selected target disk per node, with destructive wipe consent;
- kubeadm bootstrap using the published Kubernetes bundle named in the install
  guide; and
- node-local runtime configuration for the domains exposed by
  `katlctl config render-node`.

Release claims extend only to the exact VM and physical-hardware paths named in
that release's retained evidence. The automated capable-host path uses libvirt,
KVM, and OVMF. A successful VM run is not a general hardware compatibility
claim. Firmware, storage controllers, network devices, and physical machines
not named in release evidence are unverified.

Katl prepares kubeadm-ready nodes and performs bounded bootstrap operations. It
does not provide a Kubernetes distribution. Users own DHCP/PXE infrastructure,
DNS, CNI, GitOps, storage, ingress, workload policy, monitoring, backup, and
application lifecycle.

## Artifact Trust

KatlOS release assets provide SHA-256 checksums and keyless GitHub build-
provenance attestations. Kubernetes bundles are digest-addressable OCI
artifacts with GitHub provenance. These are optional tools for operators who
want a stricter supply-chain policy; the normal home-lab path accepts readable
release and bundle versions and performs its own internal consistency checks.

The current `katlc` management API uses bearer authentication over unencrypted
TCP. Restrict port 9443 to an isolated evaluation management network. It is not
a production-grade remote-management trust boundary, even when artifact
provenance verification succeeds.

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

## Compatibility Promise

All `v1alpha1` source, bundle, operation, API, and persisted-state formats are
experimental. They may change incompatibly between alpha releases. Katl does
not promise forward or backward compatibility with another alpha, automatic
state migration, or an upgrade path from every development build. Preserve the
source `ClusterConfig`, exact release assets, checksums, OCI digests, and
recovery data. Reinstall may be required after an incompatible change.

Use the `katlctl` binary from the same KatlOS release to validate and compile
configuration. Mixing release trains is outside the tested surface unless the
release notes explicitly say otherwise.

## Upgrade And Recovery Limits

KatlOS host update and rollback are node-local root, UKI, sysext, and confext
operations. They do not roll back etcd, kubeadm mutations, Kubernetes API
objects, persistent volumes, application data, or external infrastructure.
After a partial kubeadm or Kubernetes mutation, the node may report that manual
recovery is required.

Kubernetes upgrades support an explicit serial rollout to a newer patch or the
next minor using a published Katl bundle. They do not provide automatic fleet
rollout, automatic post-mutation repair, etcd disaster recovery, failed
control-plane replacement, or general cluster reconciliation. Wipe/reinstall is
destructive recovery, not backup or same-cluster disaster recovery. Keep
independent etcd, workload, and data backups; do not rely on Katl generation
rollback as a cluster backup.

## Explicitly Unsupported

Do not use the alpha as the basis for:

- production, regulated, multi-tenant, or security-critical clusters;
- an availability or disaster-recovery commitment;
- unattended host or Kubernetes fleet upgrades;
- Secure Boot or measured-boot policy enforcement;
- hardware enablement beyond retained release evidence;
- stable API, schema, on-disk-state, or long-term Kubernetes support promises;
  or
- private artifact and credential distribution policy.

## Reporting A Problem

Open a [Katl GitHub issue](https://github.com/katl-dev/katl/issues/new/choose)
using the bug report form. Remove tokens, private keys, kubeconfigs, join
commands, and other secrets before attaching anything. Include:

- the KatlOS version, release URL, and `katlctl version` output;
- exact artifact filenames and Kubernetes OCI reference, plus SHA-256 values
  or provenance results when they are relevant and available;
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
