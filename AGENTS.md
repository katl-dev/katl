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

## Testing and Release Confidence

- Test operator-visible outcomes and durable contracts, not incidental formatting, timing, generated ordering, or implementation details. Fix systemic test design when failures reveal overspecification; do not mask flakes with retries or looser assertions.
- Design install and lifecycle behavior as typed, idempotent, testable transitions. Use unit tests for policy and planning, golden tests for generated assets, and VM tests for integrated boot and lifecycle journeys.
- Release-critical services must participate in health semantics and be asserted directly in the relevant VM journey. A successful boot marker is not proof that operator access or management paths work.
- `go test ./...` is the baseline unit and golden gate, not evidence that VM, boot, install, update, or kubeadm flows ran.
- Changes affecting VM infrastructure, fixtures, boot, installation, updates, disk layout, or kubeadm state must run the relevant `scripts/vmtest-run ... -count=1` gate on a capable host. If unavailable, record the exact capability gap and command still required.
- Verify generated systemd units with `systemd-analyze verify` where practical. Risky boot, disk, update, security, or kubeadm changes also require focused review.
- Use delete-on-success retention for routine VM gates. Keep large run output only while debugging and remove it afterwards.
- A release should exercise the same artifacts and supported journeys users receive. Do not infer release readiness solely from unit tests or a successful artifact build.
- Use `katldev` for hands-on user-journey and UX verification when operator-facing behavior changes or automated gates expose usability concerns. Exercise representative happy paths, repeat applies, online configuration changes, and safe failure or recovery paths through the public `katlctl` interface without relying on internal shortcuts. Judge the journey as a user would: verify useful progress, actionable errors, expected end state, and the absence of surprising disruption; record product gaps discovered along the way.
- Treat Katldev journey testing as exploratory product evidence, not a ritual duplicate gate. Reuse a recent, relevant completed journey when later work has not changed the exercised behavior or artifact; rerun only the portions invalidated by subsequent changes or needed to verify a discovered fix.

## Task and Delivery Workflow

- Use Beads through `bd`. Check `bd ready` or `bd list` before starting; create or claim a Bead for non-trivial work with concrete acceptance criteria, and keep its notes current when scope or gates change. Keep Beads operational data local unless explicitly directed otherwise.
- Review `git status --short` before editing, staging, and committing. Preserve unrelated user changes and stage only explicit task paths.
- Deliver changes through ready-for-review pull requests. Use drafts only when explicitly requested. Keep PR descriptions concise: state what changed, why, and the root cause when fixing a defect; omit routine local-check inventories.
- Enable auto-merge after required checks when authorized, then monitor the PR through merge. Do not treat opening a PR as completion.
- Commit with `git commit-wrapped` and provide both title and body. Titles use `area: summary`, where the summary completes “When merged, this change will …”. Bodies should concisely explain the durable change and its reason.
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
