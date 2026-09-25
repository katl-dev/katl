# Verify KatlOS Release Artifacts

This is an optional expert workflow for operators who want to authenticate
downloaded artifacts against the Katl release pipeline. It is not a prerequisite
for installing or operating KatlOS on the normal trusted home-lab path.

When using it, download every asset in a verification set from one GitHub
release; never mix files from different tags.

## Inputs

- exact KatlOS tag, such as `v2026.7.0-beta.1`;
- assets required for the chosen operation;
- `SHA256SUMS`; and
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

## Verify Integrity

Run from the directory containing the downloaded assets:

```sh
sha256sum --ignore-missing --check SHA256SUMS
```

Every downloaded file named by `SHA256SUMS` must report `OK`. An adjacent
checksum can verify one file, but it does not replace the release-wide manifest:

```sh
sha256sum --check katl-installer.iso.sha256
```

On macOS, use `shasum -a 256 --check katl-installer.iso.sha256` for the adjacent
checksum. To verify each downloaded file against `SHA256SUMS`, select its exact
filename and pipe the matching line to `shasum -a 256 --check -`.

Stop if a digest fails. Delete the mismatched file and fetch it again from the
same release. Do not edit a release artifact or its metadata.

## Verify Build Provenance

Authenticate each executable or image asset against the exact tag and Katl
release workflow:

```sh
TAG=v2026.7.0-beta.1
gh attestation verify katl-installer.iso \
  --repo katl-dev/katl \
  --signer-workflow katl-dev/katl/.github/workflows/release-artifacts.yml \
  --source-ref "refs/tags/$TAG"
```

Repeat for `katlctl`, a loose PXE artifact, or the upgrade SquashFS you will
actually use. Record the tag, source commit, filename, SHA-256, and whether
attestation verification passed.

## Confirm Release Identity

Install the matching CLI under its stable name and inspect its identity. Replace
`RELEASE_VERSION` with the selected release:

```sh
VERSION=RELEASE_VERSION
PLATFORM=linux-amd64 # Use darwin-arm64 on an Apple Silicon Mac.
install -m 0755 "katlctl-$VERSION-$PLATFORM" ~/.local/bin/katlctl
katlctl version
```

The CLI and KatlOS assets must come from the same release unless release notes
explicitly declare another combination supported.

## Trust Boundary

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

Release notes compare the standard and LTS kernels, list shared runtime package
versions, and list extensions built for the release. Package versions come from
the published `katl-*.packages.tsv` inventories, including RPM release and
architecture. Zero RPM epochs are omitted; nonzero epochs are preserved.
Kubernetes extensions are distributed separately.
