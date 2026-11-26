package efiboot

// Entries holds the observed systemd-boot EFI entry IDs. Empty means absent
// when the read succeeded.
type Entries struct {
	Default string
	OneShot string
}
