# Continuous integration

**CI** is the required check for pull requests and the merge queue, and it also
runs fast checks on main. It selects VM and driver coverage from changed paths
and fails if any selected job fails, is cancelled, or is skipped. Documentation,
maintainer release tooling, and unit-only changes need only fast checks.

## VM coverage

GitHub-hosted Ubuntu runners execute the existing libvirt suite with KVM. Each
selected kernel flavour builds immutable test inputs once and runs two lanes:

- **Lifecycle:** installation and local handoff, configuration, management
  rotation, and update/rollback.
- **Cluster:** ISO/PXE boot, storage authority, two-node bootstrap, and three-node
  installation.

VM suites run in the merge queue, and on a pull request only while it carries the
`vmtest` label. A pull request without the label runs fast checks, the Nix build,
and any selected driver qualification. Adding a label to a pull request starts a
fresh run in place of the one in progress, so add `vmtest` early in the life of a
branch. Pushes to main run no VM or driver jobs, because the merge queue already
tested the same tree.

Every selected test must emit a Go JSON pass event. A labelled pull request and
the merge queue run the complete suite for both standard and LTS whenever any
VM suite is selected. Driver qualification runs wherever it is selected, on pull
requests and again in the merge queue, so the queue qualifies the combination of
changes it will merge without relying on earlier pull request results. Kernel
lock changes select the affected driver flavour, and shared boot, builder,
module, runtime package, and VM infrastructure changes select both. Manual CI
dispatch treats every path as changed, so it runs both flavours with driver
qualification for both.

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

## Build caches

Merge queue runs restore build caches but do not save them: their refs are never
restored by other runs and would only consume cache quota. The scheduled
**Cache warm** workflow builds the shared VM images for both flavours on main
nightly, which refreshes the BuildKit builder layers (`mkosi-builder`) and the
`hosted-vmtest-build-*` Go and package caches that pull requests and the merge
queue restore from main. Release artifact caches (`katl-go-runtime-*`,
`katl-go-installer-*`, `katl-mkosi-packages-*`) are saved only by Release
Artifacts.

## Kernel updates

**Kernel Updates** captures signature-verified RPM sets, retains the immutable
inputs, verifies anonymous retrieval, and opens a ready-for-review PR. Retention
stores inputs; it does not promote a kernel. CI owns qualification, on the pull
request and again in the merge queue: for each
changed flavour it builds both boot artifacts and every release extension,
verifies module composition, and exercises the production DRBD driver through
upgrade, replication, and removal, alongside the regression lanes, which the merge queue runs. Shared ABI and
extension recipe changes require driver qualification for both flavours.

After pull-request CI succeeds, **Merge Qualified Kernels** enables native auto-merge only
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
workflow routing, which today means manual dispatches and the scheduled cache
warm on main. Before enabling it, enforce trusted workflow/ref admission
outside PR-editable YAML and allocate at most one VM runner per node because
vsock CIDs share the host kernel. Qualify the deployed pool manually before
setting `KATL_VMTEST_MAIN_RUNNER=katl-vmtest`.

## Release promotion

**Katl Release Artifacts** runs on release branches, tags, or manual dispatch.
Merged revisions are qualified by the merge queue without a second release-artifact build. Releases run
regression for both flavours and qualify the exact final production bundle,
including installation, reboot, upgrade from the previous release, and rollback
without a test agent. Publication consumes those validated bytes. See
[Release KatlOS](releases.md) for the promotion contract and failure handling.
