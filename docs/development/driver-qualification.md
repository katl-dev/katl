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
| Active Fedora x86_64 kernel lock and production code in `cmd/katl-kernel-release`, `internal/kernelrelease`, and `internal/flavour` | The selected flavour's exact kernel RPMs and how the builder resolves and stages them. The other flavour and inactive Fedora locks are outside the digest. A change to a kernel lock still forces that flavour. |
| `extensions/*/recipe.json`, kernel-extension mkosi profiles, `scripts/build-release-extensions`, `scripts/assemble-release-extensions`, and the artifact, release extension, image mounting, and kernel module packages | Recipe discovery, pinned sources and patches, build hooks, OCI assembly, extension-release generation and validation, and the merged `/usr/lib/modules` tree. This includes the package containing `release_extensions.go` and the code that builds, exports, merges, and indexes modules. |
| Builder container, container trust policy, mkosi builder action and sandbox, `scripts/fedora-release`, `scripts/mkosi`, `mkosi.conf` and shared fragments | Distribution, architecture, repository and build configuration, and the execution of the selected mkosi targets. The root config projection omits comments and local cache and workspace settings. The whole mkosi wrapper covers generated kernel config, module cleanup, UKI construction, and image dispatch, along with its other targets. |
| Runtime build hooks, mkosi settings and component lock, installer and endpoint-advertiser profiles, runtime RPM specs and build tools | The workflow builds the runtime and the installer twice, and packages the endpoint-advertiser extension into both the test install and production upgrade images. The profiles, package lists, locks, and hooks are conservatively hashed in full where their settings affect both driver inputs and general image assembly. |
| `scripts/build-katlos-install-image`, `scripts/build-endpoint-advertiser-sysext`, image assembly and bundle packages, and the artifact-index command | The two image roles share an assembly path. The production upgrade image carries the release extension closure, and the test install image supplies the fixture pinned by the VM runner. |
| Runtime `os-release.in`, checked-in extension-release files in the selected profiles, module paths and module loader configuration | The release files contribute the matching fields `ID`, `VERSION_ID`, `SYSEXT_LEVEL`, `ARCHITECTURE`, `SYSEXT_SCOPE`, `SYSEXT_ID`, `CONFEXT_LEVEL`, and `KATL_*`. The Go generators contribute their code. Runtime kernel and module loader files, the builder's module export rules, and the module-composition code determine where drivers are installed and loaded. |
| Production Go code for `katlctl`, the node agent, generation activation, and the extension-aware configuration and image packages, plus the relevant sysext units and capability marker | DRBD uses `context save`, `node upgrade --apply-config`, and `cluster apply` to add, retain, and remove the extension. These paths select the driver, stage its bundles and module indexes, and activate it on install, live apply, or boot. The shared packages are conservatively hashed at package scope because the entry points share parsing, request, and execution code. |
| Driver workflow, hosted apt and VM setup, VM execution scripts, VM-test and resource-lock commands, DRBD tools profile, and the DRBD test with its shared VM code and test helpers | The runner pins the test install image, starts the guests, and requires a pass event from the named replication test. The workflow projection keeps build and test settings and drops the summary step, diagnostic collection and upload steps, and the log retention and debug-on-failure environment variables. The hosted VM setup action and runner wrappers are hashed in full because they also configure the host and run the test. |
| `go.mod` and `go.sum` | The module graph, Go setup, and dependency checksums used by build tools, the agent, CLI, and test runner. They are conservatively hashed as whole files, so mixed-purpose dependency edits can invalidate qualification. |

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
list excludes those files, package unit tests, and Markdown. When changing the
fingerprint format or its interpretation, bump the version so records made
under the earlier interpretation expire.

## Qualification records

The driver job writes its event SHA and input digest beside the committed kernel
lock details in its job summary. After the selected driver jobs succeed on a
main push, on the schedule, or on a manual dispatch on main, the publisher
writes a successful status on that exact SHA for each selected flavour. The
status context is `katl/driver-qualification/<flavour>` and the description is
the versioned digest. A commit status survives the expiration of the diagnostic
artifact. Only the publisher receives `statuses: write`.

The planner walks main's first-parent commits and reads status contexts in
batches. An intervening commit that skipped a driver job leaves earlier
evidence available. A flavour with verified matching evidence appears in
`qualifiedDrivers` with the qualifying SHA and is absent from
`driverFlavours`. A missing, unreadable, or unsuccessful record leaves the
affected flavour selected. GitHub lookups have a 20-second budget. Concurrent
plans can both select a flavour before its first record is published.

Manual dispatch and scheduled runs bypass reuse for both flavours. A kernel lock
change bypasses reuse for the affected flavour. Pushes to release branches
continue to use the release-branch push plan. The aggregate CI check requires
`skipped` for an empty driver matrix and `success` for a non-empty one.
