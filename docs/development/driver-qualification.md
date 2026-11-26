# Driver qualification

CI builds and assembles the DRBD9 and NVIDIA release extensions against the
selected kernel, then runs the DRBD replication journey on two installed nodes.
The planner uses a versioned digest to decide whether a flavour already has
qualification evidence for the candidate's inputs. The weekly schedule at
05:37 UTC on Mondays requalifies both flavours against the hosted environment
and available upstream inputs.

## Input coverage

The [planner's input list](../../cmd/katl-vmtest-plan/driver_inputs.go) defines
the digest. It hashes the flavour and sorted paths, modes, and content from
the candidate's Git tree. Renaming or deleting a selected file, or changing its
executable bit, changes the digest. For selected symlinks, the digest also
includes their in-tree targets.

| Input | What it controls |
| --- | --- |
| Active Fedora x86_64 kernel lock | The exact kernel RPMs for each flavour. |
| Extension recipes, kernel-extension mkosi profiles, extension build and assembly scripts, kernel and extension Go packages, and the associated artifact commands | Kernel RPM preparation, exported kernel inputs, recipe compilation, release extension metadata and composition, and validation of the combined module tree. |
| mkosi wrapper, install and upgrade image assembly script, builder container and action, root and installer mkosi configuration, and runtime build, configuration, and finalization files | The toolchain, kernel export, and the installer, runtime, and upgrade images used in qualification. The root and installer config projections omit comments and local cache and workspace settings. |
| Runtime release identity, module paths, and module loader configuration | The matching fields in `os-release` and `extension-release.*` and the locations where the system installs and looks up drivers. |
| Generation activation and module-index code, installer extension bundles and image metadata, config apply extension code, sysext units, and activation settings | Staging a release-owned extension, activating it when the generation boots, and updating module indexes when the extension changes. |
| Driver workflow's build and test settings, the DRBD test and its fixture helpers, and the DRBD tools mkosi profile | Installation of the test nodes, replication and resync, repeat apply, driver removal, and inspection of the running kernel and backing disks. The workflow projection excludes record publication and diagnostics. |
| Go module directives, versions, and checksums | The Go version, toolchain directives and module pins. Moving a requirement between direct and indirect blocks preserves the digest if its version and other directives are unchanged. |

General CLI and agent code, unrelated VM suites and runner code, and general
installer, endpoint-advertiser, and runtime package changes are covered through
the other CI suites and periodic Drivers runs. Bump the digest version when
changing its format or interpretation to expire records from earlier versions.

## Qualification records

A main run or a merge-group run targeting main publishes a pending status
against the SHA it tests, followed by success or failure for each selected
flavour. The status context is `katl/driver-qualification/<flavour>/<digest>`.
Its description is the versioned digest, and its target URL links to the exact
CI run attempt. A successful merge-group status is already on main's commit
when GitHub lands that same SHA, so the main push can adopt it with the original
run URL intact, including after a kernel lock change.

The planner reads matching contexts across main's first-parent history and
uses the most recently published outcome, with priority for a verified
merge-group success on the main push SHA itself. When timestamps tie,
failure takes precedence over pending, which takes precedence over success.

The planner verifies the GitHub Actions bot as the creator and checks the
run attempt against the repository, CI workflow, event, branch, and commit.
It finds the executable Drivers job in that attempt by the flavour and
digest in its display name. A success record requires the matching job to
have completed successfully. If the selected record comes from a merge
group whose head differs from the main push SHA, that flavour runs again.

A pending record defers Drivers while the matching job is queued or in
progress without a conclusion and the attempt started at most 90 minutes
ago. Otherwise Drivers runs again. GitHub lookups have a 20-second budget,
and concurrent plans can select the same flavour before a pending record
appears.

The Plan step logs the full digest, record SHA, context, state, producer URL,
decision, and reason for each flavour in stderr and the step summary.
`qualifiedDrivers` names the qualifying commit for a verified success.

Manual dispatch and scheduled runs force both flavours. A kernel lock change
forces the affected flavour unless an exact merge-group success already
qualifies the main push. Release-branch pushes use the release-branch push
plan. The aggregate CI check requires `skipped` for an empty driver matrix and
`success` for a non-empty one.
