# Development scripts

Find the rest of the [development tools](../dev/README.md) in the tool index.

## `codex-role`

Launch a Codex job from the task worktree as a systemd user service. The
launcher adds the role instructions and shared agent workflow to the brief
and waits for the service to exit. Use the [agent workflow](../dev/agents/README.md)
to choose the role and effort for a task.

```text
scripts/codex-role <delivery|reviewer|debugger|rebase> [--effort LEVEL] [--log PATH] [--bind DIR | --ro-bind PATH | --env NAME]... [--] <task-name> <task-text-or-@file>
```

| Role | Model | Default effort | Codex sandbox |
| --- | --- | --- | --- |
| `delivery` | `gpt-6.2-sol` | medium | workspace-write |
| `reviewer` | `gpt-6.2-astra` | medium | read-only |
| `debugger` | `gpt-6.2-astra` | high | read-only |
| `rebase` | `gpt-6.1-terra` | medium | workspace-write |

Choose a task name of up to 255 bytes including the
`katl-codex-ROLE-` prefix and `.service` suffix. The task name itself
must contain at least one character from `[A-Za-z0-9:_.-]` and use only
those characters. Pass a nonblank brief as text or as `@FILE`, where
`FILE` is readable by the launcher. An active unit with the same name
blocks a second launch. Wait for it or choose a different task name.

```sh
scripts/codex-role delivery issue-791 @brief.md
scripts/codex-role reviewer --effort high --ro-bind /tmp/review-evidence.txt \
  review-791 @review.md
```

Put options before the task name. `--effort LEVEL` overrides the default
reasoning effort. `--log PATH` selects a report file in an existing
directory, with relative paths resolved from the launch directory. Use
`--bind DIR` for an extra writable service mount and `--ro-bind PATH`
for a read-only file or directory. Both can be repeated. The unit has a
private `/tmp` and masks the home directory. Explicit binds reject all
of `/tmp`, the home directory or its ancestors, and paths resolving
inside the home directory. Refer to the resolved paths in the brief
when you bind symlinks. The Codex sandbox still controls writes from
the job.

The service gets the worktree, linked Git metadata when needed, Go caches,
`CODEX_HOME` (default `~/.codex`), and the log directory. Use
`--env NAME` to pass a selected exported variable, repeating it for
additional names. The launcher reserves `GH_TOKEN`, `GITHUB_TOKEN`,
`GH_ENTERPRISE_TOKEN`, and `GITHUB_ENTERPRISE_TOKEN`. The shared
[job environment](job-environment) sets `KATLCTL_CONFIG` to a path inside
the worktree and rejects a configured path outside it.

### Provision GitHub read access

Jobs can launch without GitHub authentication. For authenticated reads, create
a fine-grained personal access token with resource owner `katl-dev` and
repository access limited to `katl`. Grant read-only access to Contents,
Issues, Pull requests, and Actions. GitHub grants Metadata read access
automatically. From the maintainer's session outside the worktree, prepare
a file and use an editor to put only the token in it.

```sh
token_file="${XDG_CONFIG_HOME:-$HOME/.config}/katl-dev/codex-gh-token"
mkdir -p -- "${token_file%/*}"
(umask 077; touch -- "$token_file")
chmod 0600 -- "$token_file"
```

The launcher requires a readable regular file with mode `0600` and
a nonempty token. It hides the host file from the unit and passes a
read-only GitHub CLI config through an encrypted systemd credential.
If the file is missing, the launch notice gives its path and the job
runs anonymously. For a credential error, check the file and use the
development shell, which provides `jq` and `gh`.

Every role's Codex command sandbox enables network access. With the token
provisioned, run the following from inside a job to verify that the repository
read succeeds and GitHub refuses a label update with HTTP 403. The request
submits the existing description so an unexpectedly writable token would
leave the label's description unchanged.

```sh
(
  set -e
  gh api repos/katl-dev/katl --jq .full_name
  label=repos/katl-dev/katl/labels/bug
  payload=$(gh api "$label" --jq '{description}')
  if response=$(printf '%s\n' "$payload" | gh api --include -X PATCH "$label" --input -); then
    printf 'GitHub accepted the label update\n' >&2
    exit 1
  fi
  printf '%s\n' "$response"
  printf '%s\n' "$response" | grep -Eq '^HTTP/[0-9.]+ 403([[:space:]]|$)'
)
```

### Find and recover a job

The default report path is
`${XDG_STATE_HOME:-$HOME/.local/state}/katl-dev/logs/katl-codex-ROLE-TASK.log`.
The launcher prints the path. The service overwrites the report and
ends it with `exit=N` after Codex exits.

```sh
systemctl --user list-units 'katl-codex-*'
journalctl --user -u katl-codex-delivery-issue-791.service
```

If the report has no exit line, check the journal for startup failures
or interruptions. The journal remains available after systemd collects
the unit.

## `hermetic`

Run commands such as Go tests inside Bubblewrap with private home and
temporary directories and an allowlisted environment. The wrapper needs
Linux user namespaces and `bwrap`, which the development shell provides.

```text
scripts/hermetic [--bind PATH | --ro-bind PATH | --env NAME]... -- CMD [ARGS...]
```

From a Git worktree, run the unit and golden gate.

```sh
nix develop -c scripts/hermetic -- go test ./...
scripts/hermetic --ro-bind /tmp/katl-test-inputs --env TEST_FIXTURE -- go test ./...
```

The second example makes an existing fixture directory visible and passes
`TEST_FIXTURE` if you exported it. Repeat `--ro-bind PATH` or
`--bind PATH` for extra read-only or writable directories or files, and repeat
`--env NAME` for exported variables. Put command arguments after the
required `--`.

The checkout, linked Git metadata when needed, and Go build and module
caches are writable. The [job environment](job-environment) sets Go
telemetry off and requires `KATLCTL_CONFIG` to resolve inside the
worktree, defaulting to `_build/katlctl/config.yaml` if unset. The
wrapper refuses missing paths, binds that expose all of `/tmp`
or the home directory, and invalid environment variable names.

Output and exit status come from the command. The wrapper creates no
report file. If `bwrap` is missing, enter the development shell. If
Bubblewrap cannot create a user namespace, check whether the host permits
unprivileged user namespaces. Run `scripts/check-fast` and
[`vmtest-run`](#vmtest-run) directly. Both invoke this wrapper for their Go
tests.

## `katl-dev-evidence`

Use `katl-dev-evidence` from the development shell to manage evidence
that needs to survive the worktree or session. The store is
`~/.local/state/katl-dev/evidence`, with `~` taken from your passwd
entry. The store path must use real directories at every level.

Create an entry and copy retained files to the printed path.

```sh
entry=$(katl-dev-evidence new 822/ci-logs)
cp -- downloaded-ci.log "$entry/"
katl-dev-evidence list
```

`list` reports each entry's logical file size, age since the entry
directory was last modified, and owner. To keep an entry
past the normal 14-day age limit, use
`katl-dev-evidence keep 822/ci-logs --until YYYY-MM-DD` with the date
you need. The pin lasts through that date in local time. `keep` moves
the entry and prints its new path, so update any consumers of the old
path. You can refer to an entry by its stable `owner/name` ID or by
its current absolute path.

When you finish with an entry, run `katl-dev-evidence release 822/ci-logs`.
If the pin is still active, `release` reports when it expires. To delete
the entry before then, run `katl-dev-evidence release 822/ci-logs --force`.
`release` and `prune` remove read-only directory trees that you own. They
refuse a path that another user owns, such as one a rootless container
created, and name that path and its owner.
Use `katl-dev-evidence prune` to trim the store to its default budget of
50 GiB, or supply a separate target with `--budget 20GiB`. Pruning counts
pinned data toward the budget and removes unpinned entries oldest first.
If the store is over its default budget, `new` directs you to release
evidence before allocating another entry.

When `list` or `new` cannot measure an entry, it reports the error and
continues. `list` shows `unknown` in the size column for that entry, and
`new` checks the budget using the entries it could measure.

`release` and `prune` report mounted directories and leave their contents alone. If
an expired pin's original path has been recreated, `prune` reports both
paths and continues processing other entries. Use the reported absolute paths
to identify each copy.

### Configure automatic cleanup

Enable `programs.katl-dev.enable` and run `home-manager switch` to configure
the private evidence store. The module's user tmpfiles rules and the evidence
tool both use `~/.local/state/katl-dev/evidence` under the passwd home,
whatever `XDG_STATE_HOME` is set to. The rules expire evidence after 14 days
based on file and directory modification times and exclude the `.pinned`
subtree.
When copying a tree of older evidence into a new entry, use
`cp -R --reflink=auto` so the copy starts with fresh timestamps.

The module runs `katl-dev-evidence prune` before periodic user tmpfiles
cleanup. Pruning returns expired pins to the ordinary tree even when the
store is below budget. Age-based cleanup proceeds if pruning fails.

To let Claude clean up evidence without a permission prompt, the maintainer
adds these recommended Bash allow rules.

- `Bash(katl-dev-evidence release:*)`
- `Bash(katl-dev-evidence prune:*)`

[tmpfiles.d reference](https://www.freedesktop.org/software/systemd/man/latest/tmpfiles.d.html)

## Check runtime build reproducibility

From the checkout you want to verify, run the comparison with the regular
mkosi builder and development shell.

```sh
nix develop --option builders '' -c scripts/check-runtime-reproducibility
```

The check uses the HEAD commit time as `SOURCE_DATE_EPOCH` unless you set it.
It clears the incremental image cache in a dedicated directory under `_build`,
builds the runtime root twice with identical arguments, and verifies that the
second build reuses the first build's cache. A difference in file contents,
paths, modes, numeric owners, modification times, or link targets fails the
check and prints a diff. The printed directory holds both manifests, both build
logs, and the build outputs.

The wrapper uses `image-cache` for its default incremental cache. If you set
`KATL_MKOSI_CACHE_DIR` to a cache used by an older wrapper, use a fresh directory
or run `scripts/mkosi runtime -f clean` before rebuilding the runtime there.

## `vmtest-run`

Run enabled installer, installed-runtime, and Kubernetes scenarios through
the libvirt world from the development shell. The runner prepares artifacts,
records host capabilities, and passes your package patterns and test flags
to `go test` through `scripts/vmtest-exec`.

```sh
KATL_VMTEST_KEEP=always scripts/vmtest-run ./internal/vmtest \
  -run '^TestFirstInstallTargetDiskBootSmoke$' -count=1 -timeout 35m
```

| Runner option | Effect |
| --- | --- |
| `-h`, `--help` | Print usage and exit. |
| `--flavour=LIST` | Run a local gate for `standard`, `lts`, or `standard,lts`. |
| `--affected` | Run the groups CI's affected-suite planner selects. Defaults to both kernel flavours. |
| `--no-rebuild`, `--no-auto-rebuild` | Use existing artifacts without running the default build steps. |
| `--rebuild`, `--auto-rebuild` | Enable the default build steps. Use `--rebuild=BOOL` or `--auto-rebuild=BOOL` to set the policy explicitly. |
| `--artifact-set=SET` | Select `default` or `install` for install and lifecycle artifacts, `runtime` for direct-runtime tests, or `none` to skip the default build targets. `--runtime-artifacts` selects `runtime`. |
| `--debug-on-failure`, `--no-debug-on-failure` | Enable or disable preserving failed domains. Use `--debug-on-failure=VALUE` for an explicit Boolean or `auto` (the default, enabled outside CI). |
| `--` | Forward all following arguments to Go. |

Rebuild values accept `1`, `true`, `yes`, `on`, and their false
forms `0`, `false`, `no`, `off`. The debug policy accepts the
same values case-insensitively. Without an explicit Go timeout, tests
get 90 minutes. The runner serializes packages and rejects `-p` above
1 because its resource admission depends on that ordering. To run
packages concurrently, use separate invocations.

Single runs use the checkout's `_build/mkosi` unless
`KATL_MKOSI_BUILD_DIR` selects another directory. The runner uses that
directory for both building and verifying current artifacts. Remove
individual artifact path overrides or
`KATL_ALLOW_STALE_INSTALLER_ARTIFACTS` before rerunning.
Use `default` or `install` for installed-runtime tests so they have
the KatlOS install image. Wait for a run to finish before starting a
`katldev` image build that uses the same directory. Kubernetes scenarios
extract kubeadm from the sysext with `fsck.erofs --extract` and use
Skopeo to cache the images it lists as an OCI archive. The development shell
provides both tools, as does Ubuntu 24.04 with `erofs-utils` and `skopeo`.
A cold image cache requires access to the Kubernetes container registry.

Use a host with system libvirt access, an active network and storage pool
(both named `default` unless overridden), and permission to define
scenario NAT networks. Libvirt 9.7 or later supports their ownership
metadata. Provide readable OVMF pflash images at `KATL_OVMF_CODE` and
`KATL_OVMF_VARS` and the devices required by the tests, including
`/dev/kvm`, `/dev/vhost-vsock`, and `/dev/net/tun`. Inspect missing
capabilities in `host-capabilities.json`. To select a different libvirt
setup, set `KATL_VMTEST_LIBVIRT_URI`, `KATL_VMTEST_LIBVIRT_NETWORK`, or
`KATL_VMTEST_LIBVIRT_STORAGE_POOL`. For the focused scenario commands,
see [capable-host gates](#capable-host-gates).

### Gate both kernel flavours

From one checkout, run the same focused test on standard and LTS in parallel.

```sh
KATL_VMTEST_KEEP=always scripts/vmtest-run --flavour standard,lts \
  ./internal/vmtest -run '^TestFirstInstallTargetDiskBootSmoke$' -count=1
```

To run the groups CI's affected-suite planner selects, commit your changes and
ensure your local `origin/main` is up to date. The gate compares `HEAD` with
the merge base of the two revisions, and refuses to start while the working
tree has uncommitted changes, because the runs would test changes the plan
does not see.

```sh
KATL_VMTEST_KEEP=always scripts/vmtest-run --affected
```

The affected gate runs each selected package and test pattern on both flavours
and exits successfully without starting a run if the plan is empty. Pass
`--flavour standard` or `--flavour lts` to select one flavour. Each flavour
reuses its own build directory, `_build/gate/FLAVOUR/mkosi`, and fixture
cache, `_build/gate/FLAVOUR/vmtest`, so `katldev` builds into `_build/mkosi`
do not affect a gate. The gate holds scripts/mkosi's build lock for each
build directory until its runs finish, and refuses to start if another gate
or build holds it. Leave the build, cache and run identity overrides unset so
the gate can allocate them. Gates build in a container; `KATL_CONTAINER_RUNTIME=direct`
supports only `_build/mkosi`.

The combined summary lists the run IDs and test outcomes by flavour. If a group
fails, the gate finishes the remaining groups and returns a failure. For each
run ID, check the retained `test-summary.txt` for the tests the gate reports.

### Read run evidence

The runner prints the run ID and directory, by default
`${TMPDIR:-/tmp}/katl-vmtest/RUN-ID`, and stores the fixture cache in
`_build/vmtest` by default. Go runs with a masked home and a temporary
directory inside the run, which keeps guest inputs visible to host libvirt.
The test launcher sets a run-local `katlctl.yaml`. VM tests use `vmtest.Katlctl.Command`
to give each test its own config and home.

Inspect `run.json` for the revision, uncommitted diff digest, and result,
`go-test.log` for the Go test event stream, `test-summary.txt` for
which tests ran, and `scenarios/` for scenario results. An exit code
of 0 from Go alone is insufficient. The runner checks for passes matching
the requested tests and, by default, for a scenario `result.json`.
If it reports that tests did not run, check the package, `-run` pattern,
and capability-related skips.

`KATL_VMTEST_KEEP=failed` (the default) keeps failed runs and compacts
passed runs to evidence such as logs, manifests, and scenario results.
Use `always` to retain full evidence or `never` to remove the run
directory. Older marked runs under the default parent are pruned by age
(14 days by default, set by `KATL_VMTEST_EVIDENCE_DAYS`). For a failing
scenario, `preserved-vm-debug.txt`, when present, gives the retained
domain, serial log, console and cleanup commands. Use a narrow scenario
with `--debug-on-failure` while debugging, then use
[`vmtest-clean`](#vmtest-clean) to remove retained state.
If evidence must outlive that cleanup, create an entry with
[`katl-dev-evidence new`](#katl-dev-evidence) and copy the needed files
to the returned path before cleaning the run.

### Capable-host gates

Run the applicable gates directly on a capable development or release host.
For a local gate, set `KATL_VMTEST_KEEP=always` and check the named passes
in the run's `test-summary.txt` before cleanup.
The installer-media gate includes a complete ISO install and observes the
installer's autonomous reboot into generation 0; the short ISO and PXE checks
remain fast boot-media diagnostics:

```sh
scripts/vmtest-run --artifact-set=runtime ./internal/vmtest \
  -run '^TestDirectRuntimeVMTestAgentSmoke$' \
  -count=1 -failfast -timeout 20m
scripts/vmtest-run --artifact-set=default ./internal/vmtest \
  -run '^TestFirstInstallTargetDiskBootSmoke$' \
  -count=1 -failfast -timeout 35m
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios \
  -run '^(TestInstallerISOBootSmoke|TestInstallerPXEBootSmoke|TestInstallerISOFirstInstallStorageAuthority)$' \
  -count=1 -failfast -timeout 35m
scripts/vmtest-run --artifact-set=default ./internal/vmtest \
  -run '^TestInstalledRuntimeConfigApplyModesSmoke$' \
  -count=1 -failfast -timeout 45m
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios \
  -run '^TestInstalledRuntimeTwoNodeOperationBackedBootstrapSmoke$' \
  -count=1 -failfast -timeout 60m
scripts/vmtest-run --artifact-set=default ./internal/vmtest \
  -run '^TestInstalledRuntimeSysupdateRootUKITransfer$' \
  -count=1 -failfast -timeout 45m
KATL_KUBERNETES_VERSION='<source-version>' \
KATL_VMTEST_KUBERNETES_UPGRADE_VERSION='<target-version>' \
KATL_VMTEST_KUBERNETES_BUNDLE='<digest-pinned-source-bundle>' \
KATL_VMTEST_KUBERNETES_UPGRADE_BUNDLE='<digest-pinned-target-bundle>' \
scripts/vmtest-run --artifact-set=default ./internal/vmtest/scenarios \
  -run '^TestKubeadmUpgradeOperationSmoke$' \
  -count=1 -failfast -timeout 75m
```

The Kubernetes upgrade gate must use deliberately selected, digest-pinned
source and target OCI references. It proves etcd snapshot capture,
control-plane-first kubeadm upgrade, live sysext replacement with a kubelet-only
restart, durable generation promotion without reboot, worker upgrade, and final
cluster health. Record the selected bundle pair with the retained gate evidence.

If the Kubernetes upgrade gate uses published bundles, supply both the
source and target references. The runner rejects a one-sided pair.

The focused external-worker regression uses one Katl control-plane VM and an
independently provisioned [external worker](../docs/operations/external-workers.md). It
removes kubeadm cluster configuration and stops the control plane's Katl
management agent before joining the worker, then checks readiness, repeat
operations, and cross-node Service traffic through the public interfaces:

```sh
scripts/vmtest-run ./internal/vmtest/scenarios \
  -run '^TestExternalWorkerJoin$' -count=1 -v -timeout 30m
```

To check all enabled scenarios on a capable host, run
`scripts/vmtest-run ./... -count=1` from the development shell. The runner
writes `world.json` and `host-capabilities.json` before handing off to Go.
Use the [CI guide](../docs/development/ci.md) to see which gates run in GitHub.

## `vmtest-clean`

Clean up a retained VM test run from outside the target, using the script
from a trusted `origin/main` checkout. Run it by absolute path as a
standalone command.

```text
scripts/vmtest-clean <run-dir|result.json>
```

```sh
/path/to/trusted/katl/scripts/vmtest-clean /tmp/katl-vmtest/RUN-ID
```

Replace `RUN-ID` with the directory printed by the runner. For a
single scenario result, pass its `result.json`. The script reads
preserved debug targets from that file, stops and undefines their
domains, and releases eligible networks once domains are gone. It
leaves the result file and run directory for full-run cleanup. For a
run directory, the script processes its results and confirms the recorded
domains are gone before deleting the directory and its evidence. Repeating
cleanup on an already removed run succeeds.

`katl-devclean` checks that the run is a direct child of a directory
named `katl-vmtest`, that the run itself is a real directory, and
that it has a regular `.katl-vmtest-run-dir` marker. It refuses
symlink targets and directories the runner did not mark, including
pre-existing custom run directories. Supply the original runner-created
path. The script also refuses unsafe domain names and empty libvirt URIs
in result records. For a libvirt failure, check the reported URI and
domain state, then retry cleanup with the original evidence. A single
result with no preserved domains has nothing for this command to clean.

The script reports progress and failures to stderr. File removal uses
the shared [cleanup safeguards](#cleanup-safeguards).

## `worktree-clean`

Retire a linked Git worktree after handing off any untracked files you
need. Invoke the script by absolute path from outside the target, as
a standalone command from a trusted `origin/main` checkout.

```text
scripts/worktree-clean <worktree-path>
```

```sh
/path/to/trusted/katl/scripts/worktree-clean /path/to/finished/worktree
```

`katl-devclean` refuses the main worktree, the checkout used to run
the tool, and the target containing the working directory or script.
It checks Git's registration and refuses a locked worktree, an unregistered
path, or a `.git` file that points to a different checkout. It also
refuses tracked edits and staged changes. Untracked files, including build
output, can be removed along with the worktree.

The tool fetches `origin` to check that the worktree's HEAD is reachable
from a remote branch. If it finds unique commits, it requires GitHub to
confirm that the exact HEAD is the head of a merged pull request. If the
fetch or required lookup fails, restore access and retry. Commit and push
changes you need to preserve. For a locked worktree, check why it was locked
before running the `git worktree unlock` command in the refusal.

Git removes the directory and its registration. The tool deletes the local
branch only if its checked tip is in `origin/main`, in a same-named remote
branch, or is the exact head of the verified merged PR. A moved branch
or one checked out elsewhere is kept. Check stderr for the cleanup result,
branch disposition, and any paths left behind.

### Cleanup safeguards

Both cleanup scripts use `katl-devclean` to check the identity and
mounts of the target before removing files. It refuses mount points,
including bind mounts found in the host or rootless Podman namespace,
and reports where they must be unmounted. Removal stays inside the
verified tree and handles subuid-owned output through the rootless
Podman namespace. Keep the run marker in place for retries.

A file-removal refusal exits 2. Partial removal exits 1 and reports
remaining paths. Resolve the reported failure and rerun if the target
still has its registration or run marker. Follow the
[repository cleanup rules](../AGENTS.md#testing-and-release-confidence)
and report anything the scripts cannot remove.
