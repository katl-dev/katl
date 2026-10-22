# Continuous integration

**CI** is the required pull-request check and also runs on main. It runs fast
checks, selects VM and driver coverage from changed paths, and fails if any
selected job fails, is cancelled, or is skipped. Documentation, maintainer
release tooling, and unit-only changes need only fast checks.

## VM coverage

GitHub-hosted Ubuntu runners execute the existing libvirt suite with KVM. Each
selected kernel flavour builds immutable test inputs once and runs two lanes:

- **Lifecycle:** installation and local handoff, configuration, management
  rotation, and update/rollback.
- **Cluster:** ISO/PXE boot, storage authority, two-node bootstrap, and three-node
  installation.

Every selected test must emit a Go JSON pass event. Ordinary product PRs use
standard; kernel lock changes use the affected flavour. Shared boot, builder,
module, runtime package, and VM infrastructure changes use both standard and
LTS. Main product changes and release regression always use both flavours.
Main relies on the driver qualification required before merge instead of
rebuilding the same driver closure again. Manual CI dispatch includes driver
qualification for both flavours.
Recognized CLI changes select only the affected scenarios within those lanes;
unknown product changes receive the complete suite.

Kubernetes producer changes select the newest supported Kubernetes candidate.
The shared image job verifies and stages that bundle, and the cluster lane
exercises it. The separate scheduled **Kubernetes Bundle Artifacts** workflow
owns discovery and publication, rather than repeating the PR image build.

Use **Hosted VM scenarios** to dispatch the complete suite for a source commit
and kernel flavour. The default runner is `ubuntu-24.04`.
`KATL_VMTEST_RUNNER` can select a supported hosted provider. Run output is deleted
on success; small diagnostic artifacts expire after three days. All image
builders share the same Docker layer cache; build input identities still
control artifact and package-cache reuse.

## Kernel updates

**Kernel Updates** captures signature-verified RPM sets, retains the immutable
inputs, verifies anonymous retrieval, and opens a ready-for-review PR. Retention
stores inputs; it does not promote a kernel. PR CI owns qualification: for each
changed flavour it builds both boot artifacts and every release extension,
verifies module composition, and exercises the production DRBD driver through
upgrade, replication, and removal, alongside the regression lanes. Shared ABI
and extension recipe changes require driver qualification for both flavours.

After CI succeeds, **Merge Qualified Kernels** enables native auto-merge only
for the unchanged, same-repository, App-authored, kernel-lock-only revision.
Branch protection controls the merge. These virtual machines do not qualify
NVIDIA hardware.

Install an automation GitHub App on this repository with **Contents** and
**Pull requests** write access, and **Checks** and **Actions** read access. Set
the repository variable `KATL_AUTOMATION_CLIENT_ID` to its client ID and the
repository secret `KATL_AUTOMATION_PRIVATE_KEY` to its private key. Keep these
credentials in the trusted proposal and promotion jobs; PR test jobs use the
read-only built-in token. See [Creating a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).

The optional `katl-vmtest` ARC pool is restricted to trusted main revisions by
workflow routing. Before enabling it, enforce trusted workflow/ref admission
outside PR-editable YAML and allocate at most one VM runner per node because
vsock CIDs share the host kernel. Qualify the deployed pool manually before
setting `KATL_VMTEST_MAIN_RUNNER=katl-vmtest`.

## Release promotion

**Katl Release Artifacts** runs on release branches, tags, or manual dispatch.
Main is covered by CI without a second release-artifact build. Releases run
regression for both flavours and qualify the exact final production bundle,
including installation, reboot, upgrade from the previous release, and rollback
without a test agent. Publication consumes those validated bytes. See
[Release KatlOS](releases.md) for the promotion contract and failure handling.
