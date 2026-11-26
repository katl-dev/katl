# Driver qualification

The CI planner runs driver qualification for each kernel flavour whose
candidate inputs lack a matching successful qualification on main. The
[planner's input list](../../cmd/katl-vmtest-plan/driver_inputs.go) defines the
versioned digest from the checked-out commit. It hashes the flavour and sorted
Git paths, modes, and content hashes, so a rename, deletion, or lost executable
bit changes the digest. The digest represents repository inputs, including
their source pins. The weekly schedule at 05:37 UTC on Mondays requalifies
both flavours against the hosted environment and available upstream inputs.

## Input coverage

| Input | What it controls |
| --- | --- |
| Active Fedora x86_64 kernel lock | The selected flavour's exact kernel RPMs. Each flavour uses its own active lock. A change to a kernel lock still forces that flavour. |
| `extensions/*/recipe.json`, kernel-extension mkosi profiles, `scripts/build-release-extensions`, and `scripts/assemble-release-extensions` | Recipe discovery, pinned sources and patches, build hooks, OCI assembly, extension-release generation and validation, and the merged `/usr/lib/modules` tree. |
| Builder container, container trust policy, mkosi builder action and sandbox, `scripts/fedora-release`, `scripts/mkosi`, `mkosi.conf` and shared fragments | Distribution, architecture, repository and build configuration, and the execution of the selected mkosi targets. The root config projection omits comments and local cache and workspace settings. The whole mkosi wrapper covers generated kernel config, module cleanup, UKI construction, and image dispatch, along with its other targets. |
| Runtime build hooks, mkosi settings and component lock, installer and endpoint-advertiser profiles, runtime RPM specs and build tools | The workflow builds the runtime and the installer twice, and packages the endpoint-advertiser extension into both the test install and production upgrade images. The profiles, package lists, locks, and hooks are conservatively hashed in full where their settings affect both driver inputs and general image assembly. |
| `scripts/build-katlos-install-image`, `scripts/build-endpoint-advertiser-sysext`, image assembly and bundle packages, and the artifact-index command | The two image roles share an assembly path. The production upgrade image carries the release extension closure, and the test install image supplies the fixture pinned by the VM runner. |
| Runtime `os-release.in`, checked-in extension-release files in the selected profiles, module paths and module loader configuration | The release files contribute the matching fields `ID`, `VERSION_ID`, `SYSEXT_LEVEL`, `ARCHITECTURE`, `SYSEXT_SCOPE`, `SYSEXT_ID`, `CONFEXT_LEVEL`, and `KATL_*`. The Go generators contribute their code. Runtime kernel and module loader files, the builder's module export rules, and the module-composition code determine where drivers are installed and loaded. |
| Sysext activation units, their in-tree symlink targets, and the configuration capability marker | The activation and reload services use shared systemd drop-ins. The digest tracks both the links and the configuration files they reference. |
| Driver workflow, hosted apt and VM setup, VM execution scripts, and DRBD tools profile | The runner pins the test install image, starts the guests, and requires a pass event from the replication test. The workflow projection keeps build and test settings and drops status publication, summaries, diagnostic collection and upload, and log retention and debug-on-failure settings. |
| Go entry points, their transitive build and test dependencies, `go.mod`, and `go.sum` | The planner runs `go list -deps` for the image and hosted binaries and adds `-test` for the DRBD test package. This includes installer planning, extension assembly, `katlctl`, activation, embedded files, and the DRBD test helpers. Repository files contribute committed content and modes. External files contribute module versions and content hashes. Go's standard library is outside the digest, and the module files are hashed in full. |

Recipe discovery reads `extensions/*/recipe.json`. BIRD's README and
`extension.env`, its standalone system-extension profile, and the
`build-system-extension` wrapper belong to the BIRD workflow. The
`build-kernel-extension` wrapper also has independent callers. Driver
qualification calls the same Go builder through `build-release-extensions`.
The kernel candidate and proposal sources belong to the kernel-update workflows.
The flavour lock and generated mkosi include configure LTS builds, so the
legacy `mkosi.kernel-lts/` cache pattern is outside this input list.

General runtime image assets such as `katlc-agent.service`, SSH and console
units, networking, and Kubernetes configuration belong to the production
build, boot, lifecycle, cluster, and service-security gates. The digest covers
the driver activation units and code reached by the DRBD journey. General job
routing in `ci.yml` and planner code determine when the job runs. The
`hosted-vmtest-diagnostics` script collects failure evidence, and
`hosted-vmtest-package` builds runner artifacts for other workflows. The input
list excludes those files, tests outside the compiled VM test package, and
documentation that isn't embedded. When changing the
fingerprint format or its interpretation, bump the version so records made
under the earlier interpretation expire.

## Qualification records

The driver job writes its event SHA and input digest beside the committed kernel
lock details in its job summary. A main push, scheduled run, or manual dispatch
on main writes a pending commit status before each selected flavour starts.
When that flavour finishes, its publisher writes success or failure on the
same SHA. If cancellation prevents publication, the pending status keeps the
digest unqualified.

The status context is `katl/driver-qualification/<flavour>/<digest>` and the
description is the versioned digest. The publishing jobs use the GitHub Actions
token and link to the exact CI run attempt. The planner verifies the creator
and checks that GitHub identifies that attempt as a CI workflow run in this
repository, on main, at the recorded SHA. Commit statuses survive the
expiration of diagnostic artifacts.

The planner reads matching status contexts across main's first-parent history
in batches and uses the most recently published outcome. An intervening commit
that skipped a driver job leaves earlier evidence available. A later pending,
failed, or unverifiable result makes the flavour run again. Verified success
appears in `qualifiedDrivers` with the qualifying SHA. GitHub lookups have a
20-second budget. Concurrent plans can select the same flavour before its
record is published.

Manual dispatch and scheduled runs bypass reuse for both flavours. A kernel lock
change bypasses reuse for the affected flavour. Pushes to release branches
continue to use the release-branch push plan. The aggregate CI check requires
`skipped` for an empty driver matrix and `success` for a non-empty one.
