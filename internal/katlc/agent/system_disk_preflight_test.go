package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// decoyDiskRunner reports the running system disk vda and, when decoyLabel is
// set, a disk vdb with serial decoy carrying that partition label.
func decoyDiskRunner(decoyLabel string) ToolRunner {
	return func(_ context.Context, argv []string, _ func(int)) ToolResult {
		switch argv[0] {
		case "lsblk":
			devices := `{"name":"vda","path":"/dev/vda","type":"disk","serial":"root","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vda5","path":"/dev/vda5","type":"part","size":1073741824,"ro":false,"partuuid":"state-part","partlabel":"KATL_STATE","mountpoints":["/var"]}]}`
			if decoyLabel != "" {
				devices += fmt.Sprintf(`,{"name":"vdb","path":"/dev/vdb","type":"disk","serial":"decoy","size":68719476736,"ro":false,"mountpoints":[],"children":[{"name":"vdb1","path":"/dev/vdb1","type":"part","size":1073741824,"ro":false,"partuuid":"decoy-part","partlabel":%q,"mountpoints":[]}]}`, decoyLabel)
			}
			return ToolResult{Stdout: []byte(`{"blockdevices":[` + devices + `]}`)}
		case "findmnt":
			return ToolResult{Stdout: []byte(`{"filesystems":[{"source":"/dev/vda5","target":"/var","fstype":"ext4","options":"rw"}]}`)}
		case "ip":
			return ToolResult{Stdout: []byte(`[]`)}
		default:
			return ToolResult{Err: fmt.Errorf("unexpected command %q", argv[0]), ExitStatus: 1}
		}
	}
}

func TestSelectGenerationRefusesLabelMountingRuntimeBesideKatlDisk(t *testing.T) {
	for _, test := range []struct {
		name, runtime, decoy string
		refused              bool
	}{
		{name: "earlier runtime beside a state partition", runtime: "2026.11.1", decoy: "KATL_STATE", refused: true},
		{name: "earlier runtime beside an ESP", runtime: "2026.11.1", decoy: "KATL_ESP", refused: true},
		{name: "earlier prerelease beside a state partition", runtime: "2026.11.0-beta.1", decoy: "KATL_STATE", refused: true},
		// Development builds are taken to have the generator.
		{name: "local build beside a state partition", runtime: "2026.11.1-local.1a2b3c4", decoy: "KATL_STATE"},
		{name: "development build beside a state partition", runtime: "0.0.0-dev", decoy: "KATL_STATE"},
		{name: "earlier runtime beside an unrelated partition", runtime: "2026.11.1", decoy: "u-data"},
		{name: "earlier runtime alone", runtime: "2026.11.1"},
		{name: "generator runtime beside a state partition", runtime: "2026.11.2", decoy: "KATL_STATE"},
		{name: "later runtime beside a state partition", runtime: "2027.1.0-beta.1", decoy: "KATL_STATE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestServer(t)
			writeCleanGenerationZeroState(t, server.Root)
			spec, _, err := generation.ReadGeneration(server.Root, "generation-0")
			if err != nil {
				t.Fatal(err)
			}
			spec.GenerationID = "earlier"
			spec.Confexts = nil
			spec.Boot.LoaderEntryPath = "loader/entries/katl-earlier.conf"
			spec.Root.RuntimeVersion = test.runtime
			state, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, spec.CreatedAt)
			if err != nil {
				t.Fatal(err)
			}
			if err := generation.WriteGeneration(server.Root, spec, state); err != nil {
				t.Fatal(err)
			}
			before, err := generation.ReadBootSelection(server.Root)
			if err != nil {
				t.Fatal(err)
			}
			server.volumeDevices = hermeticVolumeDevices(t)
			server.RunVolumeDiscovery = decoyDiskRunner(test.decoy)
			mutated := false
			server.MountBootRoot = func(context.Context, string) error { mutated = true; return fmt.Errorf("stop after preflight") }
			request := &agentapi.GenerationMutationRequest{GenerationId: "earlier", ExpectedEnrollmentId: testEnrollmentID, ExpectedInventoryNodeName: testInventoryNodeName, ExpectedMachineId: testMachineID, ExpectedCurrentGenerationId: "generation-0"}

			_, err = server.SelectGeneration(context.Background(), request)

			if !test.refused {
				if !mutated {
					t.Fatalf("SelectGeneration() = %v, want preflight to pass", err)
				}
				return
			}
			if status.Code(err) != codes.FailedPrecondition || mutated {
				t.Fatalf("SelectGeneration() = %v (boot entries reached: %t), want a refusal before any switch", err, mutated)
			}
			for _, part := range []string{"/dev/vdb (serial decoy)", test.decoy, "detach", "2026.11.2"} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("SelectGeneration() error = %v, want it to name %q", err, part)
				}
			}
			after, err := generation.ReadBootSelection(server.Root)
			if err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("boot selection = %+v, want unchanged", after)
			}
		})
	}
}
