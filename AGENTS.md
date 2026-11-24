# Agent Guidelines

Katl produces KatlOS, an installable, upgradeable, systemd-native Kubernetes node OS. Keep work inside that product boundary and do not preserve compatibility for designs that have not shipped.

## Product Principles

- Optimize the supported journey for a home-lab operator on a trusted network. Routine install, configuration, bootstrap, upgrade, and management should be direct and unsurprising.
- Do not expose internal metadata, artifacts, or verification steps as operator ceremony. Calculate and carry integrity information internally. Keep stricter provenance or pinning optional for users with a different threat model.
- Default routine operations to the least disruptive safe behavior. Require confirmation only for genuinely destructive or ambiguous actions, and make errors explain what the operator should do next.
- Treat local, remote, and automation paths as one product experience. Every supported interface needs clear state, progress, and recovery guidance.
- Keep release artifacts self-contained for supported journeys. Avoid hidden prerequisites or separately distributed implementation artifacts when Katl can package, discover, or generate them.
- Katl prepares and manages Kubernetes nodes; it is not a Kubernetes distribution or a replacement for user-managed GitOps.
- Keep implementation details behind KatlOS workflows unless an expert interface specifically needs them.

## Engineering Boundaries

- Use Go for product policy, state machines, validation, planning, agents, and reusable logic that benefits from unit tests.
- Keep shell to small hooks and orchestration. Shell must not own installer policy, disk layout decisions, update policy, or other substantial state machines.
- Use mkosi as the image builder through `scripts/mkosi`; do not turn mkosi hooks into the installer engine.
- Use the libvirt-backed `scripts/vmtest-run` world for automated VM testing. Do not add competing VM runners without an explicit product need.
- Prefer native systemd primitives and configuration. Do not hide systemd behind a lossy abstraction; allow native passthrough where it is the clearest interface.
- Keep the runtime root immutable and versioned. Persistent identity and workload state belong on writable state storage; `/run` is ephemeral.
- Assume EFI-only boot unless a durable design decision expands the supported scope.
- Do not commit host-specific paths such as user home directories, `/nix/store`, `/run/current-system`, or `/etc/profiles`. Use `PATH`, repository-relative paths, containerized tooling, or explicit local overrides.
- Published images and release artifacts must not contain VM test agents, fixtures, or other test-only support code.

## Single Ownership

- Give every decision one owner. Node address roles, storage selection and identity, rendered configuration such as kubeadm, kubelet, API proxy and mount units, validation rules, and health definitions each have one implementation that every path calls. Install, live apply, bootstrap, join, upgrade, and rollback differ in their inputs, not in their rules.
- Before writing code that computes, renders, validates, or derives something, find the code that already owns it and use that, or move both behind one shared owner. A second implementation of the same responsibility needs a reason recorded in a design document; similar code with a genuinely different contract is not a duplicate.
- Bind a selected target once and carry the binding. After a disk, partition, or node address is selected, later steps use the bound identity and never resolve it again by label, kernel name, selector, or the current attempt's state. `docs/internal/storage-identity.md` defines this for storage.
- Treat a defect caused by two implementations disagreeing as a missing owner. Fix it by consolidating them, and search for other pairs that own the same responsibility before considering the fix complete.

## Testing and Release Confidence

- Test operator-visible outcomes and durable contracts, not incidental formatting, timing, generated ordering, or implementation details. Fix systemic test design when failures reveal overspecification; do not mask flakes with retries or looser assertions.
- Treat a check that fails on an unrelated change as a defect to root-cause and fix immediately, not as noise to rerun. Rerun such a check only while its root cause is understood and a fix is in progress.
- Keep tests and development worlds hermetic with respect to the operator's environment. Tests must not read or write the operator's git configuration, `katlctl` contexts, or home directory: give each test's git and `katlctl` invocations configuration inside the test's own directory, and keep `katldev` contexts in the world's own `KATLCTL_CONFIG`.
- Do not run a `katldev` image build while a `scripts/vmtest-run` gate uses the same checkout's `_build/mkosi`; the gate would test whatever the build leaves behind.
- Design install and lifecycle behavior as typed, idempotent, testable transitions. Use unit tests for policy and planning, golden tests for generated assets, and VM tests for integrated boot and lifecycle journeys.
- Release-critical services must participate in health semantics and be asserted directly in the relevant VM journey. A successful boot marker is not proof that operator access or management paths work.
- `go test ./...` is the baseline unit and golden gate, not evidence that VM, boot, install, update, or kubeadm flows ran.
- Changes affecting VM infrastructure, fixtures, boot, installation, updates, disk layout, or kubeadm state must run, on a capable host, the `scripts/vmtest-run ... -count=1` tests that the change adds or modifies, and the tests that directly exercise the changed behaviour. The merge queue runs the full VM regression suite for every merge group, so do not repeat whole regression groups locally unless debugging a failure there. If no capable host is available, record the exact capability gap and the command still required.
- Take a local VM gate result only from that run's own evidence. Set `KATL_VMTEST_KEEP=always`, read the `--- PASS` lines for the named tests from that run's `go-test.log`, then delete the run directory. Neither an exit status nor a shared log is evidence of which tests ran.
- Verify generated systemd units with `systemd-analyze verify` where practical. Risky boot, disk, update, security, or kubeadm changes also require focused review, which should check whether the change adds or keeps a second implementation of a responsibility that another path already owns.
- Use delete-on-success retention for routine VM gates. Keep large run output only while debugging and remove it afterwards.
- A release should exercise the same artifacts and supported journeys users receive. Do not infer release readiness solely from unit tests or a successful artifact build.

## User-Journey Validation

- Every user-facing change must pass hands-on user-journey validation in a persistent `katldev` VM through the public `katlctl` interface before it is landed. Automated `vmtest` coverage, unit tests, direct inspection of internal state, and a journey run for an earlier change do not substitute for this gate. Documentation-only changes are exempt unless they describe behavior that cannot be confirmed from the code, tests, or an earlier recorded journey; in that case, validate the specific claim in a `katldev` VM rather than the whole journey.
- Exercise the parts of the journey affected by the change, including representative happy paths, repeat operations or applies, online changes, next-boot or reboot behavior, and safe failure or recovery paths where applicable. Verify actual Linux, systemd, Kubernetes, and persisted-state outcomes from an operator-accessible surface without relying on internal shortcuts.
- Judge the journey as a user would: require useful progress, actionable errors, the expected end state, idempotence, and no surprising disruption. Record the exact commands and outcomes in the Bead. Treat a discovered product gap as a failed gate that must be fixed and rerun before landing; if the host cannot provide the required VM capability, record the exact capability gap and outstanding journey, and do not claim the user-facing change is validated.
- Validate environmental and product-state assumptions with read-only evidence before lifecycle mutations. When a journey fails, preserve and diagnose the failed state before wiping or reinstalling so recreation does not hide a root defect. Use wipe only when recovery is unsupported or cannot be made safe, and verify corrected inputs and the reinstall path before destroying the evidence.

## Task and Delivery Workflow

- Use Beads through `bd`. Check `bd ready` or `bd list` before starting; create or claim a Bead for non-trivial work with concrete acceptance criteria, and keep its notes current when scope or gates change. Add to notes with `bd update --append-notes`; `--notes` replaces the existing notes. Keep Beads operational data local unless explicitly directed otherwise.
- Review `git status --short` before editing, staging, and committing. Preserve unrelated user changes and stage only explicit task paths.
- Deliver changes through ready-for-review pull requests. Use drafts only when explicitly requested. Keep PR descriptions concise: state what changed, why, and the root cause when fixing a defect; omit routine local-check inventories.
- Enable auto-merge after required checks when authorized, then monitor the PR through merge. Do not treat opening a PR as completion.
- Commit with `git commit-wrapped`, which the development shell provides, and give both title and body. Commits are authored and committed under the operator's own identity. Titles use `area: summary`, where the summary completes “When merged, this change will …”. Bodies should concisely explain the durable change and its reason.
- Release notes use commit subjects by default. For commits that should appear under a different name, add one `Release-Note: <reader-facing summary>` trailer to the commit body. Use `Release-Note: skip` to omit internal changes. A squash-merged PR keeps its commits' trailers in the body, and the last `Release-Note:` in the body is used. Keep the summary on one line; it replaces the full displayed entry, including the area label. Use these trailers only when the default subject would mislead release readers or add noise.
- Use `docs/releases/<version>.md` when a release needs operator guidance beyond one-line change entries, such as upgrade paths, required actions, compatibility or deprecation notices, known limitations, and recovery guidance. Add it while preparing that release, after verifying the behavior and commands it describes. The release generator appends this file after the generated changes; review the resulting `RELEASE_NOTES.md` for the exact release before publishing. Link to durable operational procedures instead of duplicating them, and keep transient test evidence in Bead notes.
- Do not rewrite history, reset, discard changes, publish releases, or mutate unrelated external state without explicit authority.

Close a Bead only after this sequence:

1. Finish the scoped change.
2. Run the applicable formatting, unit, generated-asset, systemd, and VM gates.
3. Complete or request focused review when risk warrants it.
4. Recheck status and explicitly stage only scoped files.
5. Commit with `git commit-wrapped`.
6. Close the Bead with the commit, gates, review outcome, and any explicit skipped-gate reason.

## Code and Documentation

- Keep names concise, readable, and locally meaningful. Name functions for their action or result, and keep test names focused on behavior; put scenario detail in table cases.
- Keep examples consistent with Katl naming and supported operator workflows.
- Put durable architecture decisions in focused design documents, stable operator procedures in operational docs, and transient findings or command output in Bead notes.
- Record unresolved design choices explicitly rather than presenting uncertain behavior as settled guidance.
