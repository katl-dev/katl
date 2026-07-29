package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/configapply"
	"github.com/katl-dev/katl/internal/installer/manifest"
)

func TestNodeVolumeStatusReportsMountHealthAndDiagnostic(t *testing.T) {
	root := t.TempDir()
	installManifest := validVolumeStatusManifest()
	installManifest.Install.Volumes = []manifest.Volume{{
		Name: "local-hostpath", Selector: manifest.VolumeSelector{Partition: &manifest.PartitionSelector{}}, Filesystem: "xfs",
	}}
	if err := configapply.WriteGenerationManifest(root, "generation-1", installManifest); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "systemctl":
			return ToolResult{Stdout: []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\nResult=exit-code\nStateChangeTimestamp=now\n")}
		case "journalctl":
			return ToolResult{Stdout: []byte("mount: wrong fs type")}
		default:
			t.Fatalf("unexpected argv: %#v", argv)
			return ToolResult{}
		}
	}
	status, err := nodeVolumeStatus(context.Background(), root, "generation-1", runner)
	if err != nil {
		t.Fatalf("nodeVolumeStatus() error = %v", err)
	}
	if len(status) != 1 || status[0].GetName() != "local-hostpath" || status[0].GetMountPath() != "/var/mnt/local-hostpath" {
		t.Fatalf("status = %#v", status)
	}
	if status[0].GetActiveState() != "failed" || !strings.Contains(status[0].GetFailureDiagnostic(), "wrong fs type") {
		t.Fatalf("failed volume status = %#v", status[0])
	}
}

func validVolumeStatusManifest() manifest.Manifest {
	return manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Kind:       manifest.Kind,
		Node: manifest.NodeConfig{
			Identity: manifest.NodeIdentity{
				Hostname: "node-1",
				SSH: manifest.SSHIdentity{AuthorizedKeys: []string{
					"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl",
				}},
			},
			SystemRole: "worker",
		},
		Install: manifest.InstallConfig{
			WipeTarget: true,
			TargetDisk: manifest.DiskSelector{ByID: "/dev/disk/by-id/test-root"},
		},
		KatlosImage: manifest.KatlosImage{
			LocalRef: "images/katlos.raw", SHA256: strings.Repeat("a", 64), SizeBytes: 1024,
			Version: "0.1.0", Architecture: "x86_64", Role: "install",
		},
	}
}
