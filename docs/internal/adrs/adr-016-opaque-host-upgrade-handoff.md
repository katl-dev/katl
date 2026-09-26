# ADR-016: Prepare host upgrades with target code before reboot

Status: accepted for plain host upgrades. Combined configuration upgrades remain
on `host-upgrade-v2` until their inputs are part of target preparation.

Date: 2026-09-26.

## Context

A host upgrade starts on one KatlOS release and boots another. The installed
`katlc` must therefore handle an image produced after it was built. A
beta.14-to-beta.17 upgrade failed before mutation because beta.14 strictly
decoded `katlos/image.json` and beta.17 added `extensionRelease` under the same
image index version. Ignoring an unknown JSON field removes that particular
rejection, but the old agent still interprets target extensions, configuration,
module indexes, and generation state. A future required feature can break any
of those steps.

The source release must protect the active generation, stage a bootable root
and unified kernel image (UKI), and preserve a working rollback entry. The
target release has the code that understands its own extension and
configuration contracts. A routine image already contains its release-owned
extensions; an operator can also provide configuration and external extension
inputs through `katlctl`.

This decision concerns the source-agent-to-target-release boundary. The
[agent API compatibility policy](../agent-api-compatibility.md) separately
governs network methods and operation kinds.

## Decision

The target image supplies a statically linked preparation program. The source
`katlc` treats target-specific image contents as opaque and runs that program
before arming a reboot. The program reads a snapshot of the source generation
and produces a complete target generation in scratch storage. The source then
validates a small, stable boot contract, publishes the prepared candidate, and
arms a one-shot trial. Normal generation activation and boot health run after
reboot; neither needs an incomplete candidate or a target-image parser in the
source release.

The source mounts the verified target image read-only and supplies a private
copy of generation, operation, identity, and relevant Kubernetes state. It
extracts the target release's preparation program from the verified runtime
root and runs it as a transient systemd service. The service has a private
network and device view, an empty capability set, a read-only host filesystem,
and write access only to its private work directory. EFI is inaccessible.
The source gives the planner the inactive slot's partition UUID through the
handoff record; the planner does not inspect or write block devices.

The source sequence is:

1. Check the expected source generation, pending operations, architecture,
   runtime interface, and the target image's stable boot envelope. Verify the
   image identity and boot components by size and digest.
2. Snapshot the generation and operation stores plus the state needed to derive
   an unchanged configuration. The source preserves target-specific image
   metadata without decoding and rewriting it. A combined `--apply-config`
   upgrade currently uses the older operation kind.
3. Run the target preparation program in an isolated environment. Its output
   contains the complete candidate generation and a preparation receipt. A
   planning failure discards the scratch output before boot mutation.
4. Verify the receipt's source, operation, image, inactive root slot, UKI, and
   loader entry against independently derived inputs. Require regular spec and
   status files and verify the receipt's digest over the opaque candidate tree.
   Stage the root and UKI, copy the complete candidate, verify the copied tree's
   digest, sync it, and publish it atomically. Set the healthy source entry as
   the persistent EFI default before arming a one-shot target boot.
5. On the target boot, activate the already prepared generation and use boot
   health to promote or fail it. Boot health waits for a usable address on a
   non-loopback interface before promoting, in addition to checking failed
   systemd units. The source remains the persistent EFI default until target
   health succeeds. Retire private preparation inputs after publication.

The source must not interpret target extension metadata or synthesize the
target generation. The target must not add required boot fields that the
source's stable envelope cannot express. In particular, kernel command-line
and state-mount changes needed before the planner or early boot cannot be
hidden in the opaque bundle. Reject such a combined change with recovery
guidance until an explicit preboot contract supports it.

The private handoff and candidate stay outside the live generation directory
until the complete candidate is copied, checked, synced, and published by one
rename. The source does not decode the candidate during preparation or
publication. Target-specific state belongs to the target release. An older
source booted by rollback still enumerates generation records, so its readable
generation core must remain available for the supported rollback window. Put
new required semantics in versioned attachments read by the target; do not
change the meaning of a field the older source uses to select its own boot.

### Preparation ABI

The v1 preparation ABI consists of the target executable invocation, the
private handoff, and `prepare-result.json` in the private handoff directory.
The result reports its ABI version, operation and source and candidate IDs,
image digest, target runtime version and runtime artifact digest, inactive root
slot and partition UUID, UKI and loader-entry paths, and a digest of the entire
candidate directory. The source compares those values with its own verified
image and boot plan, requires regular nonempty `spec.json` and `status.json`,
and hashes the tree before and after copying. Successful target exit and a
valid receipt assert that the target has prepared a complete generation. The
source does not decode extension lists, configuration, target status semantics,
or other generation contents to make that assertion.

Additional JSON fields are optional and ignored by an older source. A new
required preboot capability needs a new ABI version or an explicit minimum
source capability; an old source must reject it before mutation. A target can
emit the oldest sufficient ABI result for a supported source and keep newer
details in separate target-owned files. ABI v1 is supported for the declared
stable upgrade window, not forever. Changes to kernel command line, state
mounting, or other actions needed before target preparation require an explicit
preboot interface update and cannot be hidden in the candidate tree.

`katlctl node upgrade --plan` must report the checks actually performed by the
target planner. The same target-owned planning path should serve the real
upgrade, so a successful plan has a meaningful relationship to the candidate
that will be staged. Revalidate source state and artifact identity before
commit because the node can change after a plan.

## Compatibility policy

A target stable release must upgrade a node on the current stable series or
either of the two preceding stable series, matching the agent API window.
Every supported target image must include a preparation program that runs on
the kernels and userspace interfaces of those source series. A target release
must be tested with frozen source agents and persisted-state fixtures from the
window through the public `katlctl` journey. Tests must cover successful
promotion, preparation failure without mutation, failed trial rollback,
repeat boot, manual rollback, and rollforward. Patch releases must not narrow
an existing path. Pre-releases do not advance the stable window; release notes
must state their supported upgrade paths.

The stable image envelope, preparation ABI, boot selection, and the minimum
generation core an older rollback source needs are part of this bounded
contract. Target-owned generation details and attachments are outside the
source's preparation ABI. Unknown descriptive metadata can be ignored when
omission cannot change the result. Unknown required capabilities must fail
before mutation with the minimum source version. New network operations follow the separate
agent API policy; a newer `katlctl` cannot make a published old agent support
a new operation kind.

This contract cannot be retrofitted into beta.14. The validated one-time bridge
uses the beta.14 `katlctl --artifact` escape hatch with unchanged target image
bytes and adjacent artifact metadata with the unsupported `extensionRelease`
field removed. See [Upgrade a KatlOS Host](../../operations/upgrade-host.md).

## Earlier prototype findings

The September 2026 `katldev` experiment used a source image
`2026.9.0-local.handoff1fixed` and a target image
`2026.9.0-local.handoff2fixed`. The target index added
`targetFeature: handoff-probe-v1` after the source image was built. This was an
experiment marker, not a proposed public image field.

| Experiment | Observation |
| --- | --- |
| Opaque source staging | Public `katlctl node upgrade --plan` and staging accepted the later image without decoding the target-only field. |
| Target-only preparation after reboot | The first trial could not read a candidate spec. The handoff and image were durable before boot, and the booted root contained the target binary, but activation skipped preparation. The cause of that lookup failure remains unresolved. |
| Failure recovery | The 10-minute deadman also required the missing spec and did not reboot. With no persistent EFI source default, a manual reboot selected the failed target again. A partial candidate directory made public generation listing fail. |
| Target code before reboot | A statically linked target planner ran on the source kernel under `unshare --mount` with an overlay `/var`. It interpreted the target-only field and wrote a complete spec, status, manifest, confext, and feature marker to scratch storage. The live source had no candidate spec. |
| Boot contract | The first full preparation failed because source and target rendered different loader-entry timestamps. Using the handoff timestamp made the entries identical. This is a stable-envelope requirement. |
| Prepared trial | After manually copying the scratch candidate into live state, public `katlctl node reboot` reached target health. Independent kernel, EFI, service, and persisted-file checks confirmed the target runtime and marker. |
| Repeat boot and rollback | A repeat boot failed because the prototype still treated a completed handoff as an armed trial. Archiving that record restored repeat-boot health. Public one-shot rollback to the source and rollforward to the promoted target both succeeded. |

The successful candidate copy and handoff retirement in that experiment were
manual. The accepted implementation moves them into the source agent with
atomic publication and isolation. The early trial and repeat-boot failures
informed the requirement for a complete candidate before reboot and an
ordinary activation path on every subsequent boot. The stable-series release
matrix remains a separate release gate.

## Alternatives considered

**Loosen source JSON decoding and keep source-owned target preparation.** This
handles optional metadata additions but leaves target extension and
configuration policy in an older agent. It does not address the repeated
cross-version failure class.

**Prepare only after booting the target root.** This lets target code interpret
its own image, but a preparation failure happens after the reboot and before
system extensions, management networking, and ordinary boot health. The
prototype exposed dependencies on a complete spec in activation, deadman,
generation listing, and fallback. Supporting this path would require a
separate pre-generation recovery state machine.

**Have `katlctl` prepare the complete candidate.** The workstation may not
have the node's effective configuration or local state. The node must still
validate boot safety and rollback state. Client-side checks can improve
diagnostics, but cannot be the sole authority for the candidate.

**Apply configuration after a plain OS upgrade.** This remains useful for
configuration domains excluded from the preboot contract, but it can require
another reboot and cannot stage a target kernel with a newly selected driver
as one generation.

## Follow-up and release gates

- Define which configuration domains and external extension bundles can join a
  target-prepared upgrade. Until then, `--apply-config` uses the earlier kind;
  plain upgrades preserve the effective configuration.
- Before the first stable release, freeze the source-kernel baseline and run
  the current-plus-two-previous-series matrix using published source agents,
  persisted-state fixtures, and actual target images. A proposed target that
  needs a newer source must fail before mutation with specific guidance.
- Bound the size and time cost of the private snapshot. The first implementation
  copies retained generations and operation records; a source-generation-only
  snapshot needs an explicit inventory of target planner inputs.
- Exercise preparation failure, crash recovery, trial failure, repeat boot,
  rollback, and rollforward in the VM gates. Preserve failed VM state before
  recovery. A trial whose management network never acquires a usable address
  must not be promoted; automatic recovery reboot policy needs a separate
  decision because an unconditional reboot can loop indefinitely.

## Consequences

Target releases own target-generation semantics, while source agents retain
a small staging and rollback contract. The target image remains self-contained
for release-owned extensions. A target program must run on supported source
kernels, and the source must provide a constrained execution environment and
verify its output. This replaces the source-owned target-generation planning
described in ADR-015 for plain host upgrades.
