package vmtest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// OldKatlMarker is a file present only on the filesystems of the disk that
// WriteOldKatlDisk builds, so a mount from that disk is visible.
const OldKatlMarker = "katl-old-disk-marker"

// OldKatlDiskLabels lists the GPT labels WriteOldKatlDisk creates, sorted.
const OldKatlDiskLabels = "KATL_ESP KATL_ROOT_A KATL_ROOT_B KATL_STATE"

// WriteOldKatlDisk builds a raw disk that carries the Katl GPT partition and
// filesystem labels of an earlier install, with fresh partition GUIDs. It
// stands in for the disk of a previous install left attached to a node. Its
// ESP and state filesystems are FAT so only mtools is needed on the host.
func WriteOldKatlDisk(ctx context.Context, workDir, disk string) error {
	type partition struct {
		label, typeGUID string
		startMiB, size  int
		format          bool
	}
	partitions := []partition{
		{"KATL_ESP", "C12A7328-F81F-11D2-BA4B-00A0C93EC93B", 1, 64, true},
		{"KATL_ROOT_A", "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709", 65, 64, false},
		{"KATL_ROOT_B", "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709", 129, 64, false},
		{"KATL_STATE", "4D21B016-B534-45C2-A9FB-5C16E091FD2D", 193, 256, true},
	}
	marker := filepath.Join(workDir, OldKatlMarker)
	if err := os.WriteFile(marker, []byte("old Katl disk\n"), 0o644); err != nil {
		return err
	}
	defer os.Remove(marker)
	commands := [][]string{{"truncate", "-s", "1G", disk}}
	table := "label: gpt\n"
	for _, p := range partitions {
		table += fmt.Sprintf("start=%dMiB, size=%dMiB, type=%s, name=%q\n", p.startMiB, p.size, p.typeGUID, p.label)
		if !p.format {
			continue
		}
		image := filepath.Join(workDir, p.label+".img")
		commands = append(commands,
			[]string{"truncate", "-s", strconv.Itoa(p.size) + "M", image},
			[]string{"mformat", "-i", image, "-F", "-v", p.label, "::"},
			[]string{"mcopy", "-i", image, marker, "::/" + OldKatlMarker},
			[]string{"dd", "if=" + image, "of=" + disk, "bs=1M", "seek=" + strconv.Itoa(p.startMiB), "conv=notrunc,sparse", "status=none"},
			[]string{"rm", "-f", image},
		)
	}
	for _, argv := range commands {
		if output, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, output)
		}
	}
	command := exec.CommandContext(ctx, "sfdisk", "--no-reread", "--no-tell-kernel", disk)
	command.Stdin = strings.NewReader(table)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("partition old Katl disk: %w: %s", err, output)
	}
	return nil
}

// SystemDiskMountsScript is a guest shell script that fails unless /, /efi and
// /var are mounted from one disk, the old Katl disk attached with serial
// katl-old is neither mounted nor changed, and its marker is not visible.
const SystemDiskMountsScript = `set -eu
parent() { lsblk --noheadings --nodeps --paths --output PKNAME "$(findmnt --noheadings --output SOURCE --mountpoint "$1")"; }
root=$(parent /)
for target in /efi /var; do
  [ "$(parent "$target")" = "$root" ] || { echo "$target is not on the root disk $root" >&2; exit 1; }
done
old=$(readlink -f /dev/disk/by-id/virtio-katl-old)
[ "$old" != "$root" ] || { echo "running root is on the old Katl disk $old" >&2; exit 1; }
if findmnt --noheadings --list --output SOURCE | grep -q "^$old"; then echo "old Katl disk $old is mounted" >&2; exit 1; fi
for marker in /efi/` + OldKatlMarker + ` /var/` + OldKatlMarker + `; do
  [ ! -e "$marker" ] || { echo "old Katl disk contents visible at $marker" >&2; exit 1; }
done
labels=$(lsblk --noheadings --output PARTLABEL "$old" | sed '/^$/d' | sort | tr '\n' ' ')
[ "$labels" = "` + OldKatlDiskLabels + ` " ] || { echo "old Katl disk partitions changed: $labels" >&2; exit 1; }
echo "system disk $root"
`
