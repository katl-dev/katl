# KatlOS release artifacts

Choose files from one [KatlOS release](https://github.com/katl-dev/katl/releases).
The files you need depend on where you run `katlctl` and how the node boots.

| Goal | Files |
| --- | --- |
| Run `katlctl` on Linux x86-64 | `katlctl-<version>-linux-amd64` |
| Run `katlctl` on an Apple Silicon Mac | `katlctl-<version>-darwin-arm64` |
| Install from USB or virtual media | `katl-installer.iso` |
| Boot the installer with a UEFI executable | `katl-installer.efi` |
| Boot the installer with a separate kernel and initial RAM disk | `katl-installer.vmlinuz` and `katl-installer.initrd` |
| Supply the OS image to a network-booted installer | `katlos-install-<version>-<arch>.squashfs` and its `.json` descriptor |
| Supply a local image for a host upgrade | `katlos-upgrade-<version>-<arch>.squashfs` and its adjacent metadata |

The ISO contains the temporary installer and the matching KatlOS image. For
network boot, serve the OS image separately. The boot files start the temporary
installer, which writes the OS image to the selected disk. Use the same
release files for every node and supply the node's configuration when it boots.

- [Install katlctl](../install-katlctl.md#install-the-release-binary)
- [Install from the ISO](../getting-started.md)
- [Install with PXE and Matchbox](../install-pxe-matchbox.md)
- [Upgrade a KatlOS host](../operations/upgrade-host.md)

The release also contains adjacent `.json` descriptors and `.sha256` files,
`SHA256SUMS`, `PROVENANCE.md`, resolved package inventories, and a release
extension inventory. Use [Verify release artifacts](../operations/verify-release.md)
if you want to check the bytes against the release manifest and verify their
build provenance.
