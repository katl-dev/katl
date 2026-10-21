# VM continuous integration

GitHub-hosted Ubuntu runners execute the existing libvirt suite with KVM. A
producer builds test inputs once; isolated jobs run boot, installation, storage,
configuration, management, update/rollback, two-node bootstrap, and three-node
installation. Every selected test must emit a Go JSON pass event. Skipped tests
cannot satisfy **VM validation**.

Use **Hosted VM scenarios** to dispatch the suite for a source commit and kernel
flavour. The default runner is `ubuntu-24.04`. `KATL_VMTEST_RUNNER` can select a
supported hosted provider. Run output is deleted on success; small failure
logs remain available in the workflow artifacts.

**Kernel Updates** qualifies both standard and LTS kernels. It builds both boot
artifacts and every release extension, verifies module composition, and runs the
VM suite for each flavour. A two-node DRBD workload also exercises each
production driver closure through upgrade, replication, and removal.
**Merge Qualified Kernels** enables auto-merge only
for the successfully qualified, unchanged revision of a bot-authored,
kernel-lock-only PR. Branch protection also requires the ordinary presubmit
checks. These virtual machines do not qualify NVIDIA hardware.

The optional `katl-vmtest` ARC pool is restricted to trusted main revisions by
workflow routing. Before enabling it, enforce trusted workflow/ref admission
outside PR-editable YAML and allocate at most one VM runner per node because
vsock CIDs share the host kernel. Qualify the deployed pool manually before
setting `KATL_VMTEST_MAIN_RUNNER=katl-vmtest`.

**Katl Release Artifacts** runs the regression suite for both flavours and
qualifies the final production bundle before publishing a tag. See
[Release KatlOS](releases.md) for the promotion contract and failure handling.
