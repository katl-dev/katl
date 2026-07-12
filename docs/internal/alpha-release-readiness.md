# KatlOS Alpha Release Readiness

Status: active release checklist for the first public alpha.

## Release Decision

The next non-development release should be `2026.7.0-alpha.1`, not a beta.

The v0.1 implementation has proved the intended lifecycle: reusable release
artifacts, installation to generation 0, multi-node Kubernetes bootstrap,
workload handoff, runtime configuration apply, KatlOS and Kubernetes update
operations, rollback, and wipe/reinstall. The remaining alpha work is primarily
the public product boundary: operator tooling must be downloadable, one source
configuration must drive the documented journey, the release must carry honest
maturity metadata, and the release-built path must be repeated with retained
evidence.

Beta would imply that users can depend on stable authoring and persisted-state
contracts, routine upgrade and recovery policy, enforced artifact trust, and a
continuously exercised capable-host matrix. Katl does not make those promises
yet.

## Intended Alpha User Journey

One repository-controlled `config.katl.dev/v1alpha1` `ClusterConfig` is the
operator source of truth:

1. Download `katlctl` and the KatlOS installer ISO from one KatlOS release.
2. Optionally authenticate the release with its checksums and GitHub build
   provenance when that matches the operator's threat model.
3. Compile the `ClusterConfig` into one Katl config bundle; Katl checks the
   source and bundle structure as part of this command.
4. Boot the same ISO on every node and select a node from that bundle through
   local handoff, seed media, or PXE URL input.
5. Reboot installed generation-0 nodes and bootstrap Kubernetes directly from
   the bundle's embedded inventory and Kubernetes selection.
6. Plan and apply later node configuration through an explicit runtime-change
   interface derived from the same source intent.

The compiled `install.katl.dev/v1alpha1` `InstallManifest` remains an internal
and advanced integration artifact. It must not be the primary getting-started
authoring format.

## Alpha Blockers

| Gap | Evidence | Owner |
| --- | --- | --- |
| No operator CLI is published with KatlOS releases | A versioned Linux amd64 `katlctl` asset is checksummed, attested, downloadable, and reports the release identity | `katl-dty.16.25.2` |
| The install guide leads with compiled per-node manifests and omits the implemented bundle inputs | ISO handoff and PXE examples use one minimal `ClusterConfig`, `katlctl config bundle`, node selection, `/v1/config-bundle`, and `katl.bundle.*` arguments | `katl-dty.16.25.3` |
| Source validation currently requires writing a bundle and no user-facing schema is discoverable | `katlctl config validate` performs a non-writing preflight and the v1alpha1 authoring schema is shipped or exposed | `katl-dty.16.25.4` |
| Canonical CalVer cannot express alpha or beta maturity | Version parsing, tag selection, workflows, and docs accept `alpha.N` and `beta.N` | `katl-dty.16.25.5` |
| Public Kubernetes bundle availability is not yet release evidence | An anonymous client fetches the selected readable tag and immutable digest and verifies its metadata/provenance | `katl-dty.16.25.6` |
| Bootstrap still asks for a separate inventory despite the bundle containing one | `katlctl cluster bootstrap` consumes a verified config bundle without duplicate topology or Kubernetes flags | `katl-dty.16.25.9` |
| Runtime apply uses a separate node-change schema with no clear derivation from source intent | The guide and CLI expose a bounded ClusterConfig/bundle-to-node runtime apply path | `katl-dty.16.25.10` |
| The complete public-asset path has not been rerun as one release gate | Released ISO, released `katlctl`, published Kubernetes bundle, install, bootstrap, workload, config apply, update/rollback, and wipe/reinstall have retained evidence | `katl-dty.16.25.7` |
| Alpha expectations are spread across development documents | The release states architecture, EFI and hardware evidence, trust boundary, compatibility promise, recovery limits, and issue-reporting evidence | `katl-dty.16.25.8` |

The capable-host workflow remains tracked by `katl-dty.13.3`. An alpha must not
claim a VM, install, update, or Kubernetes gate passed merely because
`go test ./...` passed.

## Public-UX Defects Resolved During Audit

The alpha hardening work corrected these user-facing gaps:

- release asset names and the tested immutable Kubernetes bundle reference now
  match the guide;
- bundle URL, digest, node selection, local handoff, and PXE input are
  documented around one `ClusterConfig` and one compiled bundle;
- the release includes `katlctl`, non-writing validation, and the source schema;
- bootstrap consumes the verified bundle's embedded inventory and Kubernetes
  selection;
- runtime configuration derives a bounded per-node request from the same source
  or verified bundle; and
- `SUPPORT.md` ships beside every release with the alpha boundary and issue
  evidence requirements.

The remaining release blocker is proof of the complete journey against the
exact public release artifacts, tracked by `katl-dty.16.25.7`.

## Accepted Alpha Limitations

The public, release-shipped contract is [`../support.md`](../support.md). In
summary, the alpha may ship with these explicit boundaries:

- x86-64 and EFI-only;
- experimental `v1alpha1` configuration APIs with no beta compatibility
  promise;
- no production support or production security claim;
- GitHub keyless build provenance and checksums, without Secure Boot signing or
  node-side signature policy enforcement;
- kubeadm-ready host and bounded bootstrap operations, not a Kubernetes
  distribution or add-on manager;
- user-managed DHCP, PXE, DNS, CNI, GitOps, storage, ingress, and application
  workloads;
- only hardware and VM paths named by retained release evidence;
- readable Kubernetes bundle tags on the normal path, with immutable digest
  pins available as an optional reproducibility control.

## Beta Blockers

The following work is deliberately not required to publish an alpha, but blocks
a beta:

- stable source, runtime-change, config-bundle, operation, and persisted-state
  compatibility policy beyond `v1alpha1`;
- enforced artifact signing, revocation, downgrade, and Secure Boot policy
  (`katl-dty.9.35.6`);
- defined Kubernetes release channels and support windows
  (`katl-dty.9.35.7`);
- continuously available release-grade capable-host CI
  (`katl-dty.13.3`);
- completed day-two repair and rebuild workflows (`katl-dty.11.25.8`);
- a supported update-controller contract (`katl-dty.12.9`);
- a reviewed greenfield GitOps operational readiness assessment
  (`katl-dty.11.24`);
- an explicit hardware support and compatibility matrix rather than
  evidence-only experimental coverage.

## Ship Rule

Publish `v2026.7.0-alpha.1` only when every alpha blocker above is closed or its
external capability gap is named in the release notes, the public user-journey
gate has passed against the exact release commit and artifacts, and the release
is still marked as a GitHub prerelease. Do not rename incomplete alpha evidence
as beta readiness.
