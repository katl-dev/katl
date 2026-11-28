# Verify KatlOS release artifacts

This is an optional expert workflow for operators who want to authenticate
downloaded artifacts against the Katl release pipeline. It is not a prerequisite
for installing or operating KatlOS on the normal trusted home-lab path.

When using it, download every asset in a verification set from one GitHub
release; never mix files from different tags.

## Inputs

- The exact KatlOS tag, such as `v2026.11.0`.
- The assets required for the chosen operation.
- `SHA256SUMS`.
- `PROVENANCE.md`.

For an ISO install, the minimum payload set is:

```text
katl-installer.iso
katl-installer.iso.sha256
katlctl-<version>-linux-amd64
katlctl-<version>-linux-amd64.sha256
SHA256SUMS
PROVENANCE.md
```

On an Apple Silicon Mac, use the `darwin-arm64` CLI and checksum file in place
of the `linux-amd64` files.

For a host upgrade, use the matching
`katlos-upgrade-<version>-<arch>.squashfs` plus its adjacent `.json` and
`.sha256` files.

## Verify integrity

Run from the directory containing the downloaded assets:

```sh
sha256sum --ignore-missing --check SHA256SUMS
```

Every downloaded file named by `SHA256SUMS` must report `OK`. An adjacent
checksum can verify one file, but it does not replace the release-wide manifest:

```sh
sha256sum --check katl-installer.iso.sha256
```

On macOS, use `shasum -a 256` in place of `sha256sum`. To verify each
downloaded file against `SHA256SUMS`, select the line for its exact filename
and pipe it to `shasum`. Repeat for each file, then check any adjacent
checksum the same way as on Linux:

```sh
grep '  katl-installer.iso$' SHA256SUMS | shasum -a 256 --check -
shasum -a 256 --check katl-installer.iso.sha256
```

Stop if a digest fails. Delete the mismatched file and fetch it again from the
same release. Do not edit a release artifact or its metadata.

## Verify build provenance

Authenticate each executable or image asset against the Katl release workflow
and the source ref the release was built from:

| Release | Source ref |
| --- | --- |
| 2026.11.7 and later | `refs/heads/release/<version>`, for example `refs/heads/release/2026.11.7` |
| 2026.11.6 and earlier | `refs/tags/<tag>`, for example `refs/tags/v2026.11.6` |

A release candidate is built from its release's branch, so `v2026.11.7-rc.1`
uses `refs/heads/release/2026.11.7`.

```sh
SOURCE_REF=refs/heads/release/2026.11.7
gh attestation verify katl-installer.iso \
  --repo katl-dev/katl \
  --signer-workflow katl-dev/katl/.github/workflows/release-artifacts.yml \
  --source-ref "$SOURCE_REF"
```

Repeat for `katlctl`, a loose PXE artifact, or the upgrade SquashFS you will
actually use. Record the tag, source commit, filename, SHA-256, and whether
attestation verification passed.

## Confirm release identity

Follow [Install the release binary](../install-katlctl.md#install-the-release-binary)
and compare the output of `katlctl version` with the release you checked.

The CLI and KatlOS assets must come from the same release unless release notes
explicitly declare another combination supported.

## Trust boundary

Checksums detect changed bytes. GitHub attestations bind bytes to a repository,
workflow, source ref, and commit. They do not provide Secure Boot signatures,
node-side signature enforcement, revocation, downgrade prevention, vulnerability
scanning guarantees, or a production incident-response commitment.

Kubernetes bundles have their own OCI digest and GitHub provenance. An operator
who wants exact byte identity can use a reference containing both the readable
tag and immutable manifest digest:

```text
ghcr.io/katl-dev/kubernetes:<version>@sha256:<oci-manifest-digest>
```
