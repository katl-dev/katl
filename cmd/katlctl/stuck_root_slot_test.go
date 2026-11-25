package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// refusalV20261104 is the error v2026.11.4 returns for a stuck inactive slot.
const refusalV20261104 = "host upgrade preflight: find inactive root slot: found 1 root partitions with type 4f68bce3-e8cd-4db1-96e7-fbcaf984b709 on system disk nvme0n1, want 2"

// sysfsRefusalV20261104 is the error v2026.11.4 returns on a retry in the
// same boot after a failed staging.
const sysfsRefusalV20261104 = `host upgrade preflight: read root partition types on /dev/disk/by-diskseq/1: partition 3 has PARTUUID fb9beb8d-6fac-46ca-94a0-d38cd9aeacd9, but sysfs reported "" for system disk vda; retry the upgrade`

func TestExplainHostUpgradeError(t *testing.T) {
	const wantReboot = `
Retrying the upgrade in the same boot will not work. An earlier failed upgrade left this node's kernel without the partition IDs the upgrade needs, and only a reboot restores them.

Reboot the node once, then run the upgrade again. This is one-time and per node: only nodes whose upgrade failed need it, and nothing changes for healthy nodes.`

	t.Run("stale sysfs refusal", func(t *testing.T) {
		agentErr := grpcstatus.Error(codes.FailedPrecondition, sysfsRefusalV20261104)

		got := explainHostUpgradeError(agentErr)

		if wantText := agentErr.Error() + "\n" + wantReboot; got.Error() != wantText {
			t.Fatalf("error = %q, want %q", got.Error(), wantText)
		}
		if !errors.Is(got, agentErr) {
			t.Fatalf("explained error no longer wraps the agent's error")
		}
	})

	t.Run("stale sysfs refusal reported by a failed operation", func(t *testing.T) {
		opErr := errors.New("operation op-1 finished with result failed: " + strings.TrimPrefix(sysfsRefusalV20261104, "host upgrade preflight: "))

		got := explainHostUpgradeError(opErr)

		if !strings.HasSuffix(got.Error(), wantReboot) {
			t.Fatalf("error = %q, want guidance suffix", got.Error())
		}
	})

	t.Run("stale sysfs refusal for another partition and disk", func(t *testing.T) {
		agentErr := grpcstatus.Error(codes.FailedPrecondition, `host upgrade preflight: read root partition types on /dev/nvme0n1: partition 7 has PARTUUID 0a1b2c3d-0000-4000-8000-0123456789ab, but sysfs reported "" for system disk nvme0n1; retry the upgrade`)

		got := explainHostUpgradeError(agentErr)

		if !strings.HasSuffix(got.Error(), wantReboot) {
			t.Fatalf("error = %q, want guidance suffix", got.Error())
		}
	})

	t.Run("other errors", func(t *testing.T) {
		for _, err := range []error{
			grpcstatus.Error(codes.FailedPrecondition, "host upgrade preflight: find inactive root slot: found 3 root partitions with type 4f68bce3-e8cd-4db1-96e7-fbcaf984b709 on system disk sda, want 2"),
			grpcstatus.Error(codes.FailedPrecondition, "host upgrade preflight: target KatlOS image is already installed"),
			errors.New("connection refused"),
		} {
			if got := explainHostUpgradeError(err); got != err {
				t.Errorf("explainHostUpgradeError(%q) = %q, want it unchanged", err, got)
			}
		}
		if got := explainHostUpgradeError(nil); got != nil {
			t.Errorf("explainHostUpgradeError(nil) = %v, want nil", got)
		}
	})
}

// euclean is the failure the agent reports when sysupdate refuses to retry an
// update whose root slot was written but whose UKI was not.
const euclean = "operation host-upgrade-handoff-20261125T014748Z-5adb5c34ac54 finished with result failed-needs-repair: systemd-sysupdate: exit-1: Automatically discovered root block device '/dev/vda'.\nDiscovering available instances…\nDetermining installed update sets…\nDetermining available update sets…\nSelected update '1' is already acquired and partially installed. Vacuum it to try installing again.: exit status 1; next action: inspect host-upgrade diagnostics and repair the staged generation before retrying"

func TestStuckSlotRecovery(t *testing.T) {
	const (
		find      = "find /efi/EFI/katl -maxdepth 1 -name '.sysupdate.*' -print -delete"
		dumpSlot  = `sfdisk --dump "$DISK" | awk '`
		slotMatch = `if ($0 ~ /name="katl_1"/ && (type == "64362E66-88A7-463B-B325-7AF60C81BE06" || type == "7A01D3C6-92AC-43E9-BD77-5726B602AD9D")) {`
		rootMatch = `} else if (type == "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709" || type == "B921B045-1DF0-41C3-AF44-4C6F280D3FAE") {`
		setType   = `sfdisk --no-reread --no-tell-kernel --part-type "$DISK" PARTNUMBER ROOTTYPE`
	)

	tests := []struct {
		name string
		err  error
		disk string
	}{
		{
			name: "found 1 root partition",
			err:  grpcstatus.Error(codes.FailedPrecondition, refusalV20261104),
			disk: "/dev/nvme0n1",
		},
		{
			name: "found 1 root partition in a failed operation",
			err:  fmt.Errorf("operation op-1 finished with result failed: find inactive root slot: found 1 root partitions with type 4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709 on system disk vda, want 2"),
			disk: "/dev/vda",
		},
		{
			name: "partially installed update",
			err:  errors.New(euclean),
			disk: "/dev/vda",
		},
		{
			name: "partially installed update without a discovered disk",
			err:  errors.New("systemd-sysupdate: exit-1: Selected update '1' is already acquired and partially installed. Vacuum it to try installing again."),
			disk: "/dev/DISK",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := explainHostUpgradeError(tt.err)

			if !errors.Is(got, tt.err) {
				t.Fatalf("explained error no longer wraps the original")
			}
			guide := strings.TrimPrefix(got.Error(), tt.err.Error())
			for _, want := range []string{find, "df -h /efi", "at least 100 MiB", "DISK=" + tt.disk + "\n", dumpSlot, slotMatch, rootMatch, setType, "Reboot the node, then run the upgrade again."} {
				if !strings.Contains(guide, want) {
					t.Errorf("guidance lacks %q:\n%s", want, guide)
				}
			}

			for _, banned := range []string{"findmnt", "lsblk"} {
				if strings.Contains(guide, banned) {
					t.Errorf("guidance depends on mount topology via %q:\n%s", banned, guide)
				}
			}

			// Deleting the UKI files comes first and the type is restored
			// before the reboot.
			order := []string{find, "100 MiB", "DISK=", setType, "Reboot"}
			last := -1
			for _, s := range order {
				i := strings.Index(guide, s)
				if i < 0 || i < last {
					t.Errorf("%q appears out of order in guidance:\n%s", s, guide)
				}
				last = i
			}
		})
	}

	t.Run("stale sysfs refusal gets no repair", func(t *testing.T) {
		got := explainHostUpgradeError(errors.New(sysfsRefusalV20261104))

		if strings.Contains(got.Error(), "sfdisk") {
			t.Errorf("reboot-only refusal prints the sfdisk repair: %s", got)
		}
	})
}

// TestStuckSlotSelection runs the awk program the guidance prints against
// partition tables, since the operator pipes a real dump into it.
func TestStuckSlotSelection(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("awk not installed")
	}

	guide := stuckRootSlotGuidance("/dev/vda")
	start := strings.Index(guide, "awk '") + len("awk '")
	end := strings.Index(guide[start:], "}'")
	if start < len("awk '") || end < 0 {
		t.Fatalf("guidance has no awk program:\n%s", guide)
	}
	program := guide[start : start+end+1]

	const (
		esp     = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
		root    = "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"
		pending = "64362E66-88A7-463B-B325-7AF60C81BE06"
		partial = "7A01D3C6-92AC-43E9-BD77-5726B602AD9D"
		state   = "4D21B016-B534-45C2-A9FB-5C16E091FD2D"
		data    = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
	)
	dump := func(rows ...[3]string) string {
		out := "label: gpt\ndevice: /dev/x\nunit: sectors\n\n"
		for _, r := range rows {
			out += fmt.Sprintf("%s : start=        2048, size=    16777216, type=%s, uuid=AAAA, name=%q\n", r[0], r[1], r[2])
		}
		return out
	}

	tests := []struct {
		name   string
		dump   string
		want   string
		wantOK bool
	}{
		{
			name:   "observed disk",
			dump:   dump([3]string{"/dev/vda1", esp, "KATL_ESP"}, [3]string{"/dev/vda2", root, "katl_0"}, [3]string{"/dev/vda3", pending, "katl_1"}, [3]string{"/dev/vda4", state, "KATL_STATE"}),
			want:   "half-installed root slot: partition 3\nother root slot type: " + root + "\n",
			wantOK: true,
		},
		{
			name:   "nvme naming and partial type",
			dump:   dump([3]string{"/dev/nvme0n1p1", esp, "KATL_ESP"}, [3]string{"/dev/nvme0n1p2", root, "katl_0"}, [3]string{"/dev/nvme0n1p12", partial, "katl_1"}),
			want:   "half-installed root slot: partition 12\nother root slot type: " + root + "\n",
			wantOK: true,
		},
		{
			name:   "extra data partition",
			dump:   dump([3]string{"/dev/vda2", root, "katl_0"}, [3]string{"/dev/vda3", pending, "katl_1"}, [3]string{"/dev/vda4", data, "scratch"}),
			want:   "half-installed root slot: partition 3\nother root slot type: " + root + "\n",
			wantOK: true,
		},
		{
			name: "no half-installed slot",
			dump: dump([3]string{"/dev/vda2", root, "katl_0"}, [3]string{"/dev/vda3", root, "_empty"}),
		},
		{
			name: "two other root slots",
			dump: dump([3]string{"/dev/vda2", root, "katl_0"}, [3]string{"/dev/vda3", pending, "katl_1"}, [3]string{"/dev/vda4", root, "other"}),
		},
		{
			name: "no other root slot",
			dump: dump([3]string{"/dev/vda2", pending, "katl_1"}, [3]string{"/dev/vda3", state, "KATL_STATE"}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("awk", program)
			cmd.Stdin = strings.NewReader(tt.dump)
			out, err := cmd.Output()

			if (err == nil) != tt.wantOK {
				t.Fatalf("awk error = %v, want success %v", err, tt.wantOK)
			}
			if string(out) != tt.want {
				t.Errorf("output = %q, want %q", out, tt.want)
			}
		})
	}
}
