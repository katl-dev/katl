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
  Top-level scripts that invoke mkosi, QEMU, go test, artifact checks, and
  resource-test setup.

domain libraries
  Go packages under internal/ for installer planning, config validation,
  VM worlds, resource manifests, kubeadm planning, and runtime state.
```

The intended product boundary is unchanged: `katlc` is the user-facing compiler
for configuration, install assets, and update artifacts; `katlctl` is the
operator CLI for cluster bootstrap and later operational workflows; node and
runtime behavior is implemented as typed Go state transitions. Shell may
orchestrate host tools, but it must not become the installer engine, the VM test
fixture engine, or a parallel manifest mutation layer.

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
  Invoke mkosi, podman, QEMU, go test -exec, unsquashfs, or similar host tools
  with repo defaults and environment normalization.

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
VM fixture policy, package lock policy, or artifact compatibility policy
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
validating user input, host facts, package locks, or artifact compatibility
planning disk, boot, update, kubeadm, or cluster bootstrap operations
classifying test outcomes, host capability gaps, or scenario results
maintaining durable state, retry behavior, or idempotent transitions
needing focused unit, golden, or table tests
```

If the decision is close, prefer Go when the behavior affects committed
artifacts, persisted node state, or CI pass/fail semantics. Prefer shell only
when the script is clearly glue around tools that already own the domain logic.

## Command Relationships

`katlc` is the planned compiler. It should own validation and compilation of
Katl configuration into install manifests, generated assets, update artifacts,
and any build-side plans that must be reproducible. Until `katlc` exists, narrow
Go commands may hold pieces of that behavior, but scripts should not grow a
second compiler.

`katlctl` is the operator CLI. It should consume compiled plans or explicit
operator input and perform cluster bootstrap or operational actions. VM tests may
execute `katlctl` as a black-box command, but `cmd/katlctl` must not own VM
world setup or scenario orchestration.

`katlos-install` and runtime helper commands own node-local install and runtime
state transitions. Shell can put these binaries into images or invoke them in a
test, but not reimplement their state machines.

`katl-resource-lock` owns resource-test manifests, package locks, and artifact
lock verification. Shell may call it, but package drift policy and manifest JSON
belong in Go.

`scripts/vmtest-run` is the standard enabled nspawn and VM test entrypoint. It
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
settling, but it must delegate package locks to `katl-resource-lock`, builds to
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
  Resource manifests, package locks, artifact records, summary logic, and
  deterministic resource-test validation.

internal/vmtest
  VM world manifests, host capability records, lease allocation, fixture
  builders, scenario directories, QEMU/nspawn helpers, and guest-agent clients.

internal/installer/*
  Install manifest parsing, disk planning, generated confext rendering,
  generation metadata, artifact selection, and install state transitions.

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

`scripts/mkosi-artifacts` is a compatibility wrapper for
`cmd/katl-mkosi-artifacts`. Existing smoke scripts may keep using the wrapper
until they can call the Go command directly or until the command is installed in
the developer environment.

`scripts/build-katlos-install-image` remains temporary file assembly glue. It
copies already-built runtime, boot, and sysext artifacts into the KatlOS image
root and invokes `mksquashfs`; validation, component indexes, checksums, and
artifact metadata belong to `cmd/katl-mkosi-artifacts`.

`cmd/katl-resource-lock` remains separate from local artifact metadata. It owns
resource-test manifests, package locks, and deterministic identity records used
to decide whether resource-sensitive tests are still valid. It may consume
artifacts produced by `scripts/mkosi` and indexed by
`cmd/katl-mkosi-artifacts`, but it should not become the image builder.

`katlc` is the future user-facing compiler for durable install/update artifacts.
When `katlc` exists, stable artifact packaging behavior from
`cmd/katl-mkosi-artifacts` should move behind the compiler or be called by it as
a typed internal package. Until then, `cmd/katl-mkosi-artifacts` is the narrow Go
surface for build-side artifact metadata policy.

## Current Script Migration Table

| Script | Current role | Policy action |
| --- | --- | --- |
| `scripts/mkosi` | Supported build entrypoint and containerized mkosi adapter | Keep as the top-level mkosi adapter while scaffolding. Artifact metadata and package provenance are delegated to `cmd/katl-mkosi-artifacts`; move Kubernetes repository mutation and remaining build policy into Go or mkosi config as they stabilize. |
| `scripts/vmtest-run` | Supported enabled nspawn/VM world entrypoint over `go test -exec` | Keep as the canonical developer entrypoint. Keep it thin; move fixture policy, leases, aggregation, and host policy into Go helpers or a future Go runner command. |
| `scripts/vmtest-exec` | `go test -exec` package-binary wrapper | Keep as an implementation detail of `scripts/vmtest-run`; do not document it as a developer entrypoint. |
| `scripts/katl-vm` | Direct QEMU debug wrapper | Keep temporarily for focused boot debugging. Do not add multi-node orchestration or installer policy; migrate stable QEMU behavior into `internal/vmtest`. |
| `scripts/check-mkosi-smoke` | Build-artifact boot smoke around direct QEMU | Replace with `scripts/vmtest-run` scenarios or a Go VM smoke command once the world runner covers the same proof. |
| `scripts/mkosi-artifacts` | Compatibility wrapper for local build artifact metadata | Keep temporarily as a wrapper over `cmd/katl-mkosi-artifacts`; delete once callers use the Go command or an installed command surface directly. |
| `scripts/build-katlos-install-image` | Packages runtime, sysext, and metadata into an install image | Keep temporarily as file-copy and `mksquashfs` glue. Structured validation, image indexes, checksums, and artifact metadata are owned by `cmd/katl-mkosi-artifacts`; move the remaining packaging flow behind `katlc` when the compiler exists. |
| `scripts/bind-install-manifest-image` | Mutates install manifest image references | Replace with `katlc` compile/bind behavior because it performs structured manifest mutation. |
| `scripts/check-katlos-install-image` | Validates install-image metadata and host-path hygiene | Move to a Go verifier shared with `katlc` and resource tests. |
| `scripts/check-runtime-root` | Inspects SquashFS runtime root content | Move policy checks to Go; Go may still call `unsquashfs` as the external inspector. |
| `scripts/check-runtime-boot-asset` | Validates runtime UKI metadata and command line | Move metadata and compatibility checks to Go. |
| `scripts/check-kubernetes-sysext` | Validates Kubernetes sysext metadata and payload version | Absorb into `katl-publish-kubernetes-sysext` or a Go verifier. |
| `scripts/check-mkosi-size` | Size budget checks for generated artifacts | May remain as a simple CI check while it only uses `stat`/`du`; move to Go if budgets become artifact metadata or release policy. |
| `scripts/prepare-nspawn-userspace-fixture` | Builds nspawn userspace fixture from runtime root | Move behind `internal/vmtest`/`internal/nspawntest` world fixture helpers. Keep only as a debug validator until tests no longer call it directly. |

## Devshell And Host Tools

The Nix devshell should declare host tools required by scripts and VM tests:
`jq`, `qemu`, `mkosi`, `unsquashfs`, `ip`, OVMF, systemd tools, and similar
external dependencies. Scripts should find those tools through `PATH` or
explicit `KATL_*` environment overrides. Committed config must not bake in
developer home paths, `/nix/store` paths, `/run/current-system`, or distro-local
profile paths.

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
