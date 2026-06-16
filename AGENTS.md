# Agent Guidelines

Katl is a greenfield project that produces and maintains KatlOS: an installable, upgradeable, systemd-native Kubernetes node OS. Keep changes aligned with that boundary, and do not preserve premature compatibility with designs that have not shipped.

## Project Direction

- Treat `katlc` as the KatlOS state/configuration command. It accepts user-supplied Katl YAML or configuration, validates it, compiles it into generation-scoped sysext/confext payloads, and applies, stages, reports, or rolls back that runtime state.
- Use Go for Katl product logic: `katlc`, installer state machines, node/update agents, config validation, disk/update planners, and reusable libraries that need unit tests.
- Do not write Go just to wrap `mkosi`, `podman`, hypervisor binaries, or `virsh` during early scaffolding. Start with thin scripts for build/boot orchestration and promote to Go only when the wrapper has meaningful parsing, state, or testable behavior. The current supported VM test path is the libvirt-backed `scripts/vmtest-run` world.
- Keep shell limited to small mkosi hooks and glue where a shell script is the clearest tool. Shell may orchestrate tools; it must not contain installer policy, disk layout decisions, or update state machines.
- Keep mkosi as the image builder. Do not turn mkosi hooks or build scripts into the installer engine.
- Keep mkosi and artifact production as implementation details behind KatlOS install/update flows; Katl is the OS product, not an end-user OS generator.
- Do not turn Katl into a Kubernetes distribution. Katl prepares kubeadm-ready nodes; kubeadm and user-managed GitOps take over from there.
- Do not hide native systemd configuration behind a lossy abstraction. User YAML/configuration must compile to native artifacts and allow passthrough where that is the clearest supported interface.
- Do not bake host-specific paths into committed project config. In particular, avoid `/run/current-system`, `/nix/store`, `/etc/profiles`, and user home paths. Use `PATH`, repo-relative paths, containerized builders, or explicit environment variables for local overrides.

## Scaffolding Boundary

- Early work should prove one small build, boot, or test loop at a time before expanding scope.
- Build paths should use `scripts/mkosi` or an equivalent thin container wrapper around mkosi.
- Boot paths should use `scripts/vmtest-run` for automated libvirt VM tests, with `mkosi vm` left only as a manual first-look tool when useful. Do not add a separate VM runner unless the user explicitly asks for it or the wrapper has meaningful state/parsing that needs tests.
- The first smoke check may be a simple timeout plus serial-log match for `Katl hello`.
- Keep new VM orchestration beyond the supported libvirt `scripts/vmtest-run` path, multi-node orchestration, CI/end-user publishing, and real disk installation out of first-boot scaffolding unless the user explicitly changes the scope.

## Runtime Model

- Target modern systemd primitives: systemd-boot, UKIs, systemd-repart, systemd-sysext, systemd-confext, systemd-tmpfiles, systemd mount units, and systemd health/boot-complete semantics.
- Assume EFI-only boot unless a design document explicitly expands scope.
- Keep the runtime root immutable and versioned. Persistent Kubernetes and node state belongs under writable state partitions and should be projected into expected paths with systemd mount units or bind mounts.
- Do not store persistent identity in `/run`; `/run` is for ephemeral runtime state.

## Testing Expectations

- Design installer behavior as typed, idempotent, and testable state transitions.
- Prefer unit tests for planning and validation logic, golden tests for generated assets, and libvirt VM tests for boot/install/update flows.
- Generated systemd units should be verifiable with `systemd-analyze verify` where practical.
- Changes to disk layout, boot flow, update flow, or kubeadm state handling need tests or an explicit note explaining the remaining gap.
- `go test ./...` is the baseline unit/golden gate, not proof that enabled VM, boot, install, update, or kubeadm flows ran.
- Changes that affect `scripts/vmtest-run`, `scripts/vmtest-exec`, VM test worlds, VM scenarios, fixture generation, boot/install/update behavior, or kubeadm smoke behavior must run the relevant `scripts/vmtest-run ... -count=1` gate on a capable host. If the current host cannot run it, record the exact host capability gap and the exact `scripts/vmtest-run` command that still needs to run.
- VM test output can be large. Use delete-on-success retention for routine gates, keep `/tmp/katl-vmtest` output only while it is needed for debugging, and remove retained run directories once they are no longer useful.

## Naming

- Keep identifiers concise and specific. Prefer names that add local meaning over names that repeat package, type, or file context.
- Name functions for the action or result they provide, not for every implementation detail they touch.
- Keep test names focused on the behavior under test. Avoid long sentence-style names; use table case names for detailed scenarios when that keeps the test function name short.
- Do not shorten names into unclear abbreviations. Concise still means readable to someone who has not been working in the file.

## Task Tracking

- Use Beads through the `bd` CLI for project task tracking.
- Check `bd ready` or `bd list` before starting new work.
- Create tasks with concrete acceptance criteria when adding non-trivial implementation work.
- Keep the active Bead updated while working, especially when scope changes or a gate is skipped.
- Do not close a Bead just because code or docs were edited. Closing a Bead means the completion workflow below is done.
- Keep Beads operational data local unless the project explicitly decides to publish or sync the database.

## Git Workflow

- Review `git status --short` before editing, staging, or committing.
- Only stage and commit files that are part of the current task. Do not sweep unrelated local changes into a commit.
- Prefer explicit path staging, for example `git add AGENTS.md docs/internal/initial-design.md`.
- Agents must use `git commit-wrapped` for commits and must supply both a title and a body.
- Commit titles must use the `area: summary` shape expected by `git commit-wrapped`; the summary phrase after `area:` must complete the sentence "When merged, this change will ...".
- Commit bodies must be self-contained and concise. Describe what changed in plain English, and include the reason or context when it is not obvious from the diff. Follow the Linux kernel commit-log style: make the body useful to a future reader of permanent project history, not just to the current reviewer.
- Do not create subject-only commits. If a change is too small to explain in one short body paragraph, say exactly what changed and why it is intentionally small.
- Example: `git commit-wrapped "runtime: select sysext by interface" "Record the Katl runtime interface in sysext metadata and validate it before activation. This keeps Kubernetes sysext updates decoupled from KatlOS root updates while preserving generation rollback as one unit."`
- Do not rewrite history, reset, or discard user changes unless the user explicitly asks for that operation.
- If unrelated work is present in the tree, leave it alone and mention it in the handoff if it affects verification.

## Completion Gates

- Closing a Bead has a required order:
  1. Finish the scoped code/docs change.
  2. Run the validation gates that match the change: formatting, unit tests, generated asset checks, `systemd-analyze verify`, `scripts/vmtest-run ... -count=1` for enabled VM/world scenarios, libvirt VM smoke tests, or docs review as applicable.
  3. Run or request review when the change is broad, risky, security-sensitive, boot/update-related, disk-layout-related, or kubeadm-state-related.
  4. Review `git status --short` and stage only the files for the completed Bead with explicit paths.
  5. Commit those files with `git commit-wrapped`.
  6. Close the Bead with a reason that names the commit, gates, and review outcome.
- Do not close the Bead before committing the completed task. If the user explicitly asks for no commit, or the task intentionally produces no file changes, record that exception in the Bead close reason.
- Use Beads gates when work depends on external review, long-running validation, or serialized merge coordination.
- Record skipped gates or skipped reviews in the Bead and final handoff with the reason.

## Documentation

- Keep examples using Katl naming: `katl`, `katlc`, `katlctl`, `/etc/katl`, `/var/lib/katl`, and `katl.*` kernel arguments.
- During early scaffolding, do not spend time maintaining broad project docs unless the user explicitly asks. These docs are expected to change quickly.
- Prefer Bead notes for transient findings, command results, and implementation discoveries.
- Update `docs/internal/initial-design.md` only for durable architecture decisions or explicit user requests.
- Update focused operational docs only when commands or prerequisites are stable enough that another developer can run them.
- Record unresolved design choices as open questions instead of burying uncertainty in examples.
