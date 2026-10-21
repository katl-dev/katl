# Hosted VM scenario experiments

Use the `Hosted VM scenarios` workflow to evaluate Katl's existing libvirt
journeys on standard `ubuntu-24.04` GitHub-hosted runners. The workflow is
experimental and manually invoked; it does not publish artifacts or establish
a required release gate. GitHub describes nested virtualization as
[technically possible but unsupported](https://docs.github.com/en/actions/concepts/runners/github-hosted-runners).

## Execution model

One producer builds the standard runtime with `KATL_VMTEST_IMAGE_SUPPORT=1`,
then runs `TestDirectRuntimeVMTestAgentSmoke` on that host. It builds the
installer ISO, install and upgrade images, and Kubernetes sysext once. These
instrumented images are test inputs and must never be promoted into release
assets.

Independent scenario jobs download the producer's immutable files, verify the
source commit, builder version, and SHA256 checksums, then invoke
`scripts/vmtest-run --artifact-set=install --no-rebuild`. The existing harness
records its resource manifest, creates fresh VM worlds, and owns libvirt
lifecycle. Builder caches and installed guest fixtures are excluded from the
transfer. The archive preserves file modes and sparse files.

The test groups cover these contracts:

| Group | Journey |
| --- | --- |
| Producer | Direct runtime boot and guest-agent health over vsock. |
| Boot | Installer ISO and PXE boot, including network readiness. |
| Install | First install, installed boot health, and local configuration handoff. |
| Config | Configuration apply modes, including online and next-boot behavior. |
| Management | Management credential rotation, repeat operation, and reboot. |
| Update | Host upgrade, health promotion, rollback, repeated upgrade, and preserved state. |
| Two-node | Operation-backed Kubernetes bootstrap and dependent configuration. |
| Three-node install | Independent generation-zero installation and identity across three nodes. |

Every selected top-level VM test must report a pass. A skipped test or an empty
selection fails the workflow. Existing guest assertions and timeouts remain in
effect; the workflow does not disable guest durability or security settings.

## Evidence

Download the `hosted-vmtest-*-evidence-<run-id>` artifacts for command output,
exit codes, stage timing, source identity, input checksums, and libvirt logs.
Failed worlds also retain their small log, JSON, and XML files. Guest disks are
not uploaded. Successful worlds use the harness's delete-on-success policy.
Diagnostic artifacts expire after three days; shared image inputs expire after
one day. Record conclusions and run links before they expire.

Each measured command samples whole-host available RAM and root-filesystem
usage every ten seconds. These are sampled observations, not guaranteed peaks
or runner capacity promises. GNU time's maximum resident set is a process
measurement, not the sum of concurrent guests. Actions step durations include
the upload and download costs outside the measured test commands.

## Qualification boundary

These tests qualify instrumented standard images built from the selected
source. Production release artifacts, the LTS flavor, Kubernetes version
upgrades, multi-control-plane recovery, and repeated reliability across runner
allocations need their own evidence. Hosted automation does not replace the
required public `katlctl` journey in a persistent `katldev` VM.

Keep tests optional until their failure modes, runtime, storage demand, and
reliability are understood. Use measurements to choose test groups before
provisioning paid runners or adding required checks.
