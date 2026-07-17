# ADR-004: Tooling layout keeps policy in Go and scripts as adapters

Status: accepted.

Date: 2026-06-06.

Katl is still in scaffolding, so shell scripts are useful for proving one build
or VM loop at a time. That convenience is starting to create a second tooling
surface beside the Go commands and libraries that already own Katl product
logic. This ADR defines where tools live, which top-level scripts may remain,
and when shell behavior must move to Go.

## Context

Katl has three kinds of tools today:

```text
user-facing Katl commands
  Go commands under cmd/, such as katlctl, katlos-install,
  katl-resource-lock, and runtime helper commands.

developer and CI entrypoints
  Top-level scripts that invoke mkosi, libvirt VM tests, go test, artifact
  checks, and resource-test setup.

domain libraries
  Go packages under internal/ for installer planning, config validation,
  VM worlds, resource manifests, kubeadm planning, and runtime state.
```

The intended product boundary is unchanged: `katlc` is the user-facing KatlOS
state/configuration command that turns user-supplied Katl YAML or configuration
into generation-scoped sysext/confext payloads and owns node-local stateful
operations; `katlctl` is an operator control client for cluster bootstrap and
later operational workflows. Node and runtime behavior is implemented as typed
Go state transitions. Shell may orchestrate host tools, but it must not become
the installer engine, the VM test fixture engine, or a parallel manifest mutation
layer.

## Decision

Katl tooling uses this hierarchy:

```text
cmd/
  Stable or becoming-stable binaries. Product logic and durable developer
  commands live here.

internal/
  Reusable typed logic, validators, planners, fixture builders, resource
  manifests, and test harness packages.

scripts/
  Thin repository entrypoints for external tools and short-lived compatibility
  shims. Scripts are allowed to sequence commands, normalize environment, and
  provide an ergonomic command name while the shape is still changing.

docs/internal/
  Durable design, command contracts, and migration notes. Transient run results
  and local host findings belong in task notes or generated run directories,
  not committed design documents.
```

Top-level scripts are allowed only when they satisfy at least one of these
roles:

```text
external-tool adapter
  Invoke mkosi, podman, libvirt clients, image tooling, go test -exec,
  unsquashfs, or similar host tools with repo defaults and environment
  normalization.

developer or CI aggregate
  Sequence already-typed commands for a still-moving workflow. The script may
  choose command order, but not own structured policy or result classification.

temporary compatibility shim
  Preserve an existing command while its behavior is moved into a Go command or
  internal package.
```

Shell must not own:

```text
installer, update, disk, kubeadm, or runtime state machines
structured manifest mutation or schema validation
JSON generation beyond trivial process-local glue that is explicitly temporary
resource-test result aggregation or scenario status classification
VM fixture policy, package inventory policy, or artifact compatibility policy
long-lived daemon behavior or retry/state reconciliation
```

When a script needs any of those behaviors, the behavior moves to a Go package
under `internal/` and, if it needs a command-line surface, a `cmd/katl-*` command.
The script may remain as a small wrapper while callers migrate.

Using `jq` does not make shell the owner of a Katl schema. `jq` is acceptable for
transitional local extraction, small validation checks, or process-local glue,
but durable schema-bearing JSON should be produced and validated by Go. The
current `scripts/vmtest-run` `world.json` and `host-capabilities.json` writes are
a temporary scaffolding exception; if those schemas grow or become compatibility
contracts, their construction moves to Go.

## Shell Versus Go Criteria

Use shell when the whole job is one of:

```text
call an external tool with stable repo defaults
set environment variables from the devshell or repository layout
run a short command sequence where failure handling is just "stop and print"
provide a temporary local shorthand during scaffolding
tail-call another program with caller arguments preserved
```

Use Go when the job includes:

```text
parsing or writing JSON, YAML, or systemd unit trees
mutating install manifests, artifact indexes, or resource manifests
validating user input, host facts, package inventories, or artifact compatibility
planning disk, boot, update, kubeadm, or cluster bootstrap operations
classifying test outcomes, host capability gaps, or scenario results
maintaining durable state, retry behavior, or idempotent transitions
needing focused unit, golden, or table tests
```

If the decision is close, prefer Go when the behavior affects committed
artifacts, persisted node state, or CI pass/fail semantics. Prefer shell only
when the script is clearly glue around tools that already own the domain logic.

## Command Relationships

`katlc` is the planned KatlOS state/configuration command. It should own
validation and compilation of user-supplied Katl YAML/configuration into
generation-scoped sysext/confext payloads, metadata, apply plans, status, and
rollback-aware runtime state. Until `katlc` exists, narrow Go commands may hold
pieces of that behavior, but scripts should not grow a second compiler.

`katlctl` is the operator control client. It may keep local client configuration
for communication profiles and known node details, consume explicit operator
input, submit requests to node-local `katlc`, observe returned operation IDs,
sequence bounded multi-node workflows, and relay explicit client-side outputs.
Its own persistent state is limited to communication profiles and known-node
details. It must not generate or own generation specs, generation status,
`OperationRecord`s, retry state, or any durable node lifecycle state. VM tests
may execute `katlctl` as a black-box command, but `cmd/katlctl` must not own VM
world setup or scenario orchestration.

`katlos-install` and runtime helper commands own node-local install and runtime
state transitions. Shell can put these binaries into images or invoke them in a
test, but not reimplement their state machines.

`katl-resource-lock` owns resource-test manifests, package inventories, optional strict locks, and artifact
lock verification. Shell may call it, but package drift policy and manifest JSON
belong in Go.

`scripts/vmtest-run` is the standard enabled VM test entrypoint. It
creates a world, records host capability state, exports the world environment,
and tail-calls `go test -exec scripts/vmtest-exec` with caller arguments.
Complex fixture building, lease allocation, result classification, and
capability policy should live in Go helpers used by tests or future runner
commands.

`scripts/vmtest-exec` is an implementation detail for `go test -exec`. It only
validates that a world exists, exports strict VM-test environment, runs one
compiled test binary, and prints the run directory on failure.

`scripts/check-resource-tests` is the planned CI/developer aggregate for
resource-backed tests. It may exist as a top-level script while the workflow is
settling, but it must delegate package inventory recording to `katl-resource-lock`, builds to
the mkosi adapter, VM execution to `scripts/vmtest-run`, and summary or result
classification to Go. Once its interface is stable or it owns nontrivial policy,
it should become a Go command.

Tiny transition shims such as `exec go run ./cmd/<tool> "$@"` are allowed when
they preserve a command name during migration. Each shim should have an obvious
replacement command and should be deleted once callers use the Go command
directly.

## Lower-Level Helper Placement

Lower-level helpers should not accumulate as permanent top-level scripts.

```text
internal/resourcetest
  Resource manifests, package inventories, artifact records, summary logic, and
  deterministic resource-test validation.

internal/vmtest
  VM world manifests, host capability records, lease allocation, fixture
  builders, scenario directories, libvirt helpers, and guest-agent clients.

internal/installer/*
  Install manifest parsing, disk planning, generated confext rendering,
  generation spec/status, artifact selection, and install state transitions.

cmd/katl-*
  Narrow command-line surfaces for reusable Go behavior that is not yet part of
  katlc or katlctl.
```

Test-only command binaries belong under `cmd/katl-*` when they are reusable
developer/CI tools, or under `internal/.../testcmd/` when they are fixtures for
one package. Do not add permanent nested script trees as a substitute for typed
Go helpers.

Generated output belongs under one of:

```text
build/
  Repository-local durable build and smoke artifacts.

${TMPDIR:-/tmp}/katl-vmtest/
  Hermetic VM world scratch and per-run scenario artifacts.

an explicit artifact directory
  Caller-provided output path for build, release, or test artifacts.
```

Generated run output must not be written under `cmd/*`, `internal/*`, or other
source package directories unless it is committed testdata intentionally used by
that package.

## Build And Artifact Command Relationships

During scaffolding, `scripts/mkosi` remains the top-level containerized mkosi
adapter. It selects the mkosi profile, prepares temporary repository input for
the Kubernetes sysext profile, invokes the container runtime and mkosi, and
orchestrates external packaging tools such as `mksquashfs` and `ukify`.

`cmd/katl-mkosi-artifacts` owns structured local build artifact metadata. It
writes and queries the mkosi artifact index, emits runtime root and runtime UKI
metadata, derives Kubernetes sysext package provenance from mkosi output, writes
KatlOS image indexes, and writes outer KatlOS image artifact metadata. Shell
wrappers may invoke this command during scaffolding, but they should not
assemble these JSON documents themselves.

`scripts/build-katlos-install-image` remains temporary file assembly glue. It
copies already-built runtime, boot, and sysext artifacts into the KatlOS image
root and invokes `mksquashfs`; validation, component indexes, checksums, and
artifact metadata belong to `cmd/katl-mkosi-artifacts`.

`cmd/katl-resource-lock` remains separate from local artifact metadata. It owns
resource-test manifests, package inventories, and deterministic identity records used
to decide whether resource-sensitive tests are still valid. It may consume
artifacts produced by `scripts/mkosi` and indexed by
`cmd/katl-mkosi-artifacts`, but it should not become the image builder.

`katlc` is the future user-facing KatlOS state/configuration command. Stable
artifact metadata behavior from `cmd/katl-mkosi-artifacts` may be shared with
`katlc` as typed internal packages when generation planning needs it, but mkosi
artifact packaging remains a build-side implementation detail. Until then,
`cmd/katl-mkosi-artifacts` is the narrow Go surface for build-side artifact
metadata policy.

The old `scripts/mkosi-artifacts` entrypoint is gone. Use
`katl-mkosi-artifacts` directly, or `go run ./cmd/katl-mkosi-artifacts` when a
developer checkout has not installed the command.

## VM Test Helper Disposition

The supported automated VM surface is still `scripts/vmtest-run`. It remains a
top-level entrypoint because developers and CI need one stable command for
libvirt-backed enabled VM tests. Its current shell body still owns runner setup
policy: artifact-set selection, explicit stale `KATL_*` input rejection, mkosi
build target selection, resource manifest preparation, host capability probing,
network CIDR discovery, and `world.json`/`run.json` emission. That runner setup
policy should move behind `internal/vmtest` or a narrow `cmd/katl-vmtest-run`
command when the runner is next expanded.

The current helper disposition is:

```text
scripts/vmtest-run
  Keep as the primary compatibility entrypoint. Migrate setup and world/run
  manifest policy to Go before adding more runner behavior.

scripts/vmtest-exec
  Keep as a thin go test -exec adapter. It only checks the world manifest,
  exports strict test environment, runs the compiled test binary, and reports
  the run directory on failure.

scripts/vmtest-debug
  Keep as a thin compatibility wrapper. Debug target discovery and rendering
  live in cmd/katl-vmtest-debug and internal/vmtest so retained-domain
  inspection does not depend on jq snippets in shell.

scripts/vmtest-clean
  Keep as a debug helper for now. It still owns preserved-domain cleanup and
  orphan qemu fallback policy; migrate or relocate it when cleanup semantics
  become a durable interface rather than a local debugging aid.

internal/vmtest/world.go, world_scenario.go, world_fixtures.go,
first_install_world.go, first_install_runtime_fixture.go, installed_world.go
  Keep as the Go owners for world schema validation, scenario directories,
  node allocation, fixture staging, first-install fixture production, and
  installed-runtime fixture discovery.

internal/vmtest/scenarios
  Keep two-node and three-control-plane topology and bootstrap scenario policy
  in Go tests. These paths consume world-published fixtures and should not grow
  generated wrapper scripts or hand-maintained fixture environment files.
```

First-install, installed-runtime, two-node, and three-control-plane closure
gates should be run through `scripts/vmtest-run` with the runner-created world.
They should not require generated wrapper scripts or manually exported
`KATL_INSTALLER_*`, `KATL_INSTALL_MANIFEST`, `KATL_RUNTIME_ARTIFACT`, or
`KATL_MKOSI_ARTIFACT_INDEX` fixture inputs. Runtime-only smoke paths may still
accept explicit runtime artifacts while they are outside the primary install
world path.

## Current Script Migration Table

| Script | Current role | Policy action |
| --- | --- | --- |
| `scripts/mkosi` | Supported KatlOS image build entrypoint and containerized mkosi adapter | Keep as the generic top-level mkosi adapter while scaffolding. It must not select Kubernetes payload versions, outputs, or VM fixture variants. Artifact metadata and package provenance are delegated to `cmd/katl-mkosi-artifacts`. |
| `scripts/build-kubernetes-sysext` | Explicit Kubernetes sysext producer over the generic mkosi adapter | Keep as a narrow artifact entrypoint. It owns the Kubernetes repository/profile preparation and one requested output; callers, including VM tests, supply non-release versions and output names explicitly. Move its remaining structured validation and cache policy into Go as they stabilize. |
| `scripts/vmtest-run` | Supported enabled VM world entrypoint over `go test -exec` | Keep as the canonical developer entrypoint. Keep it thin; move fixture policy, leases, aggregation, and host policy into Go helpers or a future Go runner command. |
| `scripts/vmtest-exec` | `go test -exec` package-binary wrapper | Keep as an implementation detail of `scripts/vmtest-run`; do not document it as a developer entrypoint. |
| `scripts/vmtest-debug` | Compatibility wrapper for retained-domain debug target rendering | Keep as a thin wrapper around `cmd/katl-vmtest-debug`; debug target discovery and rendering policy live in Go. |
| `scripts/vmtest-clean` | Debug helper for preserved libvirt domains | Keep as a local debug helper while cleanup semantics are still scaffolding; migrate or relocate if it becomes a durable interface. |
| `scripts/check-mkosi-smoke` | Operator-friendly build and boot smoke wrapper | Keep as a thin compatibility wrapper around `scripts/vmtest-run` so smoke execution uses the libvirt-backed VM test path. |
| `scripts/build-katlos-install-image` | Packages runtime components and metadata into an install image | Keep temporarily as file-copy and `mksquashfs` glue. Structured validation, image indexes, checksums, and artifact metadata are owned by `cmd/katl-mkosi-artifacts`; keep generic artifact packaging separate from `katlc` runtime generation apply. |
| `scripts/bind-install-manifest-image` | Compatibility wrapper for install manifest image binding | Structured artifact lookup, digest checks, manifest mutation, `localRef` validation, and target disk override live in `cmd/katl-mkosi-artifacts bind-install-manifest-image`; keep the script as a thin environment-default wrapper while callers still use the historical entrypoint. |
| `scripts/check-katlos-install-image` | Validates install-image metadata and host-path hygiene | Move to a Go verifier shared with artifact metadata tooling and resource tests. |
| `scripts/check-runtime-root` | Inspects SquashFS runtime root content | Move policy checks to Go; Go may still call `unsquashfs` as the external inspector. |
| `scripts/check-runtime-boot-asset` | Validates runtime UKI metadata and command line | Move metadata and compatibility checks to Go. |
| `scripts/check-kubernetes-sysext` | Validates Kubernetes sysext metadata and payload version | Absorb into `katl-publish-kubernetes-sysext` or a Go verifier. |

## Devshell And Host Tools

The Nix devshell should declare host tools required by scripts and VM tests:
`jq`, libvirt clients, image tooling, `mkosi`, `unsquashfs`, `ip`, OVMF,
systemd tools, and similar external dependencies. Scripts should find those
tools through `PATH` or explicit `KATL_*` environment overrides. Committed
config must not bake in developer home paths, `/nix/store` paths,
`/run/current-system`, or distro-local profile paths.

When a script needs an additional external tool, add it to the devshell before
documenting the command as agent-runnable.

## Consequences

This policy intentionally keeps a small top-level script surface during early
scaffolding while preventing scripts from becoming permanent product logic. It
also makes future cleanup mechanical: migrate structured behavior into Go,
leave a compatibility wrapper only when needed, then delete the wrapper once
callers use the Go command or the canonical runner.

The policy does not require moving every script immediately. It does require new
work to avoid adding more shell-owned policy, and it gives follow-up work a
concrete migration table for eliminating existing shell JSON generation and
complex fixture behavior.
