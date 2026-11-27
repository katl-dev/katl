package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/boothealth"
	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// systemctl show output for the settle target, katl-boot-health.service, and
// katl-boot-complete.target, as observed on systemd for each phase of a boot.
const (
	healthSettling = "Id=katl-boot-health-settled.target\nLoadState=loaded\nActiveState=inactive\nJob=2529\n\n" +
		"Id=katl-boot-health.service\nLoadState=loaded\nActiveState=activating\nJob=2530\n\n" +
		"Id=katl-boot-complete.target\nLoadState=loaded\nActiveState=inactive\nJob=2528\n"
	healthSucceeded = "Id=katl-boot-health-settled.target\nLoadState=loaded\nActiveState=active\nJob=\n\n" +
		"Id=katl-boot-health.service\nLoadState=loaded\nActiveState=inactive\nJob=\n\n" +
		"Id=katl-boot-complete.target\nLoadState=loaded\nActiveState=active\nJob=\n"
	healthFailed = "Id=katl-boot-health-settled.target\nLoadState=loaded\nActiveState=active\nJob=\n\n" +
		"Id=katl-boot-health.service\nLoadState=loaded\nActiveState=failed\nJob=\n\n" +
		"Id=katl-boot-complete.target\nLoadState=loaded\nActiveState=inactive\nJob=\n"
	healthDependencyFailed = "Id=katl-boot-health-settled.target\nLoadState=loaded\nActiveState=active\nJob=\n\n" +
		"Id=katl-boot-health.service\nLoadState=loaded\nActiveState=inactive\nJob=\n\n" +
		"Id=katl-boot-complete.target\nLoadState=loaded\nActiveState=inactive\nJob=\n"
	healthStalled = "Id=katl-boot-health-settled.target\nLoadState=loaded\nActiveState=inactive\nJob=\n\n" +
		"Id=katl-boot-health.service\nLoadState=loaded\nActiveState=inactive\nJob=2612\n\n" +
		"Id=katl-boot-complete.target\nLoadState=loaded\nActiveState=inactive\nJob=2611\n"
	healthUnitsMissing = "Id=katl-boot-health-settled.target\nLoadState=not-found\nActiveState=inactive\nJob=\n\n" +
		"Id=katl-boot-health.service\nLoadState=not-found\nActiveState=inactive\nJob=\n\n" +
		"Id=katl-boot-complete.target\nLoadState=not-found\nActiveState=inactive\nJob=\n"
)

const readyManagementLink = `{"Interfaces":[{"Name":"management0","NetworkFile":"10-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}]}`

func readReadyManagementNetwork(context.Context) ([]byte, error) {
	return []byte(readyManagementLink), nil
}

const addresslessManagementNetwork = `{"Interfaces":[
	{"Name":"management0","NetworkFile":"10-lan.network","RequiredForOnline":true,"RequiredOperationalStateForOnline":["routable","routable"],"OnlineState":"online","OperationalState":"carrier","CarrierState":"carrier","AddressState":"off"},
	{"Name":"management1","NetworkFile":"20-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}
]}`

// healthUnit fakes systemd's view of katl-boot-health.service.
type healthUnit struct{ properties atomic.Value }

func (u *healthUnit) set(properties string) { u.properties.Store(properties) }

func (u *healthUnit) run(_ context.Context, argv []string, _ func(int)) ToolResult {
	properties := u.properties.Load().(string)
	if properties == "" {
		return ToolResult{Err: errors.New("systemd query failed"), ExitStatus: 1}
	}
	return ToolResult{Stdout: []byte(properties)}
}

func (u *healthUnit) runWithFailedSSH(ctx context.Context, argv []string, progress func(int)) ToolResult {
	switch {
	case slices.Contains(argv, "--property=Requires"):
		return ToolResult{Stdout: []byte("sshd.service\n")}
	case slices.Contains(argv, "sshd.service"):
		return ToolResult{Stdout: []byte("Id=sshd.service\nActiveState=failed\n")}
	default:
		return u.run(ctx, argv, progress)
	}
}

// knownGoodNode is a node whose generation-0 passed boot health in an earlier
// boot, so persisted state alone reports it healthy.
func knownGoodNode(t *testing.T, unitProperties string) (*testServer, *healthUnit) {
	t.Helper()
	server := newTestServer(t)
	server.MountBootRoot = func(context.Context, string) error { return nil }
	root := server.Root
	writeCleanGenerationZeroState(t, root)
	spec, _, err := generation.ReadGeneration(root, "generation-0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "var/lib/katl/generations/generation-0")); err != nil {
		t.Fatal(err)
	}
	spec.Boot.LoaderEntryPath = "loader/entries/katl-generation-0.conf"
	knownGood, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, time.Date(2026, 6, 15, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, knownGood); err != nil {
		t.Fatal(err)
	}
	writeTestBootArtifacts(t, root, spec)
	writeProcCmdline(t, root, "root=PARTUUID=11111111-1111-1111-1111-111111111111 rootfstype=squashfs ro katl.generation=generation-0")
	unit := &healthUnit{}
	unit.set(unitProperties)
	server.RunBootHealthStatus = unit.run
	return server, unit
}

func TestNodeStatusTracksRequiredManagementLink(t *testing.T) {
	for _, tc := range []struct {
		name, network string
	}{
		{name: "carrier lost", network: `{"Interfaces":[
			{"Name":"management0","NetworkFile":"10-lan.network","RequiredForOnline":true,"OnlineState":"offline","OperationalState":"no-carrier","CarrierState":"no-carrier","AddressState":"routable"},
			{"Name":"management1","NetworkFile":"20-lan.network","RequiredForOnline":true,"OnlineState":"online","OperationalState":"routable","CarrierState":"carrier","AddressState":"routable"}
		]}`},
		{name: "address lost", network: addresslessManagementNetwork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := knownGoodNode(t, healthSucceeded)
			writeRebootEFIDefault(t, server.Server)
			efi := rootedPath(server.Root, efiVarsPath)
			writeTestEFIVariable(t, efi, "LoaderEntryOneShot", "katl-generation-0.conf")
			server.SetBootDefault = func(_ context.Context, _, entry string) error {
				writeTestEFIVariable(t, efi, "LoaderEntryDefault", entry)
				return nil
			}
			server.SetBootOneshot = func(_ context.Context, _, entry string) error {
				writeTestEFIVariable(t, efi, "LoaderEntryOneShot", entry)
				return nil
			}
			server.RunReboot = func(context.Context, []string, func(int)) ToolResult {
				t.Fatal("a management link outage requested a reboot")
				return ToolResult{}
			}
			files := []string{
				filepath.Join(server.Root, "var/lib/katl/boot/selection.json"),
				filepath.Join(server.Root, "var/lib/katl/generations/generation-0/status.json"),
				filepath.Join(efi, "LoaderEntryDefault"+loaderVarGUID),
				filepath.Join(efi, "LoaderEntryOneShot"+loaderVarGUID),
			}
			type snapshot struct {
				data []byte
				info os.FileInfo
			}
			readFiles := func() []snapshot {
				t.Helper()
				var snapshots []snapshot
				for _, path := range files {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					snapshots = append(snapshots, snapshot{data, info})
				}
				return snapshots
			}
			before := readFiles()

			// A second routable link must not hide the required link's outage.
			network := tc.network
			server.readManagementNetwork = func(context.Context) ([]byte, error) { return []byte(network), nil }
			for _, want := range []string{boothealth.Unhealthy, boothealth.Healthy} {
				report, err := server.GetNodeStatus(t.Context(), &agentapi.GetNodeStatusRequest{})
				if err != nil {
					t.Fatal(err)
				}
				diagnostic := report.GetBootHealthDiagnostic()
				if report.GetBootHealthState() != want {
					t.Fatalf("boot health = %s (%s), want %s", report.GetBootHealthState(), diagnostic, want)
				}
				if want == boothealth.Unhealthy && (!strings.Contains(diagnostic, "management0") ||
					!strings.Contains(diagnostic, "systemd-networkd") || strings.Contains(diagnostic, "cable") ||
					strings.Contains(diagnostic, "switch port") || strings.Contains(diagnostic, "lost carrier") || strings.Contains(diagnostic, "link down") || !strings.Contains(diagnostic, "katlctl node status")) {
					t.Fatalf("boot health diagnostic = %q, want link and repair guidance", diagnostic)
				}
				if want == boothealth.Healthy && diagnostic != "" {
					t.Fatalf("recovered boot health diagnostic = %q, want empty", diagnostic)
				}
				for i, after := range readFiles() {
					if !bytes.Equal(before[i].data, after.data) || !os.SameFile(before[i].info, after.info) ||
						!before[i].info.ModTime().Equal(after.info.ModTime()) {
						t.Fatalf("%s changed while reporting boot health %s", files[i], want)
					}
				}
				network = readyManagementLink
			}
		})
	}
}

func TestNodeStatusBootHealthFollowsCurrentBoot(t *testing.T) {
	for _, tc := range []struct {
		name, unit, want string
	}{
		{name: "settling", unit: healthSettling, want: boothealth.Pending},
		{name: "succeeded", unit: healthSucceeded, want: boothealth.Healthy},
		{name: "failed", unit: healthFailed, want: boothealth.Failed},
		{name: "dependency failed", unit: healthDependencyFailed, want: boothealth.Failed},
		// Boot health passes because the generation is sound, but the node
		// still reports the volumes the activation check left unmounted.
		{name: "volumes refused", unit: healthSucceeded + "\n\nId=katl-volumes-activate.service\nLoadState=loaded\nActiveState=failed\nJob=\n", want: boothealth.Failed},
		{name: "settle timed out", unit: healthStalled, want: boothealth.Pending},
		{name: "units unavailable", unit: healthUnitsMissing, want: boothealth.Unknown},
		{name: "query failed", want: boothealth.Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := knownGoodNode(t, tc.unit)

			nodeStatus, err := server.GetNodeStatus(context.Background(), &agentapi.GetNodeStatusRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if nodeStatus.GetBootHealthState() != tc.want {
				t.Fatalf("boot health = %q (%s), want %q", nodeStatus.GetBootHealthState(), nodeStatus.GetBootHealthDiagnostic(), tc.want)
			}
		})
	}
}

func TestRepairDuringStalledBootHealth(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	writeProcCmdline(t, server.Root, "root=PARTUUID=11111111-1111-1111-1111-111111111111 rootfstype=squashfs ro katl.generation=generation-0")
	unit := &healthUnit{}
	unit.set(healthStalled)
	server.RunBootHealthStatus = unit.run
	server.MountBootRoot = func(context.Context, string) error { return nil }
	executor := NewExecutor(server.Root, server.Store, server.AgentStartID)
	executor.Async = false
	server.Dispatcher = executor

	accepted, err := server.SubmitOperation(t.Context(), &agentapi.SubmitOperationRequest{
		ApiVersion: APIVersion, Kind: RequestKind, ClientRequestId: "repair-apply",
		OperationKind: OperationKindGenerationApply, Actor: "test-actor",
		ConfigApply: &agentapi.ConfigApplyOperationRequest{
			CandidateGenerationId: "repair",
			ApplyMode:             generation.ApplyModeAuto,
			ConfigYaml:            configApplyNoChangesYAML(),
		},
	})
	if err != nil {
		t.Fatalf("apply during stalled boot health: %v", err)
	}
	record, err := server.Store.Read(accepted.GetOperationId())
	if err != nil || record.Result != operation.ResultSucceeded {
		t.Fatalf("apply result = %q, read error = %v", record.Result, err)
	}

	writeRebootEFIDefault(t, server.Server)
	server.RunReboot = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
	if _, err := server.Reboot(t.Context(), &agentapi.RebootRequest{
		ApiVersion: APIVersion, Kind: RebootRequestKind, Actor: "test", TargetGenerationId: "generation-0",
	}); err != nil {
		t.Fatalf("reboot during stalled boot health: %v", err)
	}
}

func TestNodeStatusReportsFailedBootDependencies(t *testing.T) {
	for _, tc := range []struct {
		name, units, details string
		failed, mismatch     bool
	}{
		{name: "skipped", units: healthDependencyFailed, failed: true, details: "did not run in this boot"},
		{name: "failed with durable diagnostic", units: healthFailed, failed: true, mismatch: true, details: "does not match durable boot evidence"},
		{name: "skipped with failed volumes", units: healthDependencyFailed + "\n\nId=katl-volumes-activate.service\nLoadState=loaded\nActiveState=failed\nJob=\n", failed: true, details: "katl-volumes-activate.service failed"},
		{name: "dependency no longer failed", units: healthDependencyFailed, details: "journalctl -b -u katl-boot-health"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, unit := knownGoodNode(t, tc.units)
			if tc.mismatch {
				selection, err := generation.ReadBootSelection(server.Root)
				if err != nil {
					t.Fatal(err)
				}
				selection.BootedGenerationID = "older"
				selection.ActiveGenerationID = "generation-0"
				if err := generation.WriteBootSelection(server.Root, selection); err != nil {
					t.Fatal(err)
				}
			}
			server.RunBootHealthStatus = func(ctx context.Context, argv []string, progress func(int)) ToolResult {
				switch {
				case slices.Contains(argv, "--property=Requires") && slices.Contains(argv, "katl-boot-health.service"):
					return ToolResult{Stdout: []byte("sshd.service systemd-networkd.service\n")}
				case slices.Contains(argv, "--property=Id,ActiveState") && slices.Contains(argv, "sshd.service"):
					state := "active"
					if tc.failed {
						state = "failed"
					}
					return ToolResult{Stdout: []byte("ActiveState=" + state + "\nId=sshd.service\n\nActiveState=active\nId=systemd-networkd.service\n")}
				default:
					return unit.run(ctx, argv, progress)
				}
			}

			nodeStatus, err := server.GetNodeStatus(t.Context(), &agentapi.GetNodeStatusRequest{})
			if err != nil {
				t.Fatal(err)
			}
			diagnostic := nodeStatus.GetBootHealthDiagnostic()
			if nodeStatus.GetBootHealthState() != boothealth.Failed ||
				!strings.Contains(diagnostic, tc.details) ||
				!strings.Contains(diagnostic, "katlctl cluster apply") ||
				!strings.Contains(diagnostic, "katlctl node reboot <node>") {
				t.Fatalf("boot health = %s (%s), want failed with %q and repair guidance", nodeStatus.GetBootHealthState(), diagnostic, tc.details)
			}
			if strings.Contains(diagnostic, "sshd.service") != tc.failed || strings.Contains(diagnostic, "systemd-networkd.service") {
				t.Fatalf("diagnostic = %q, want failed dependencies only", diagnostic)
			}
		})
	}
}

func TestMutationWaitsForBootHealth(t *testing.T) {
	restoreWait := bootHealthAdmissionWait
	t.Cleanup(func() { bootHealthAdmissionWait = restoreWait })
	// Removing the running generation fails its own precondition once admitted.
	remove := &agentapi.GenerationMutationRequest{
		GenerationId: "generation-0", ExpectedEnrollmentId: testEnrollmentID, ExpectedInventoryNodeName: testInventoryNodeName,
		ExpectedMachineId: testMachineID, ExpectedCurrentGenerationId: "generation-0",
	}

	t.Run("held while boot health runs", func(t *testing.T) {
		bootHealthAdmissionWait = 50 * time.Millisecond
		server, _ := knownGoodNode(t, healthSettling)

		if _, err := server.RemoveGeneration(context.Background(), remove); status.Code(err) != codes.Unavailable {
			t.Fatalf("generation mutation = %v, want Unavailable", err)
		}
		if _, err := server.SubmitOperation(context.Background(), submitRequest("during-boot-health")); status.Code(err) != codes.Unavailable {
			t.Fatalf("operation = %v, want Unavailable", err)
		}
		dryRun := submitRequest("dry-run-during-boot-health")
		dryRun.DryRun = true
		if _, err := server.SubmitOperation(context.Background(), dryRun); status.Code(err) == codes.Unavailable {
			t.Fatalf("dry run = %v, want it evaluated", err)
		}
	})

	t.Run("admitted when boot health settles", func(t *testing.T) {
		bootHealthAdmissionWait = 10 * time.Second
		server, unit := knownGoodNode(t, healthSettling)
		var finished atomic.Bool
		go func() {
			time.Sleep(100 * time.Millisecond)
			finished.Store(true)
			unit.set(healthSucceeded)
		}()

		_, err := server.RemoveGeneration(context.Background(), remove)
		if !finished.Load() {
			t.Fatalf("mutation returned while boot health was running: %v", err)
		}
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("mutation after boot health = %v, want the generation precondition", err)
		}
	})

	for name, unit := range map[string]string{"dependency failed": healthDependencyFailed, "failed": healthFailed, "settle timed out": healthStalled} {
		t.Run("admitted when boot health "+name, func(t *testing.T) {
			bootHealthAdmissionWait = 10 * time.Second
			server, _ := knownGoodNode(t, unit)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			if _, err := server.RemoveGeneration(ctx, remove); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("repair mutation = %v, want it admitted", err)
			}
		})
	}

	t.Run("refused before the caller's deadline", func(t *testing.T) {
		bootHealthAdmissionWait = 10 * time.Second
		server, _ := knownGoodNode(t, healthSettling)
		ctx, cancel := context.WithTimeout(context.Background(), bootHealthReplyMargin+500*time.Millisecond)
		defer cancel()

		if _, err := server.RemoveGeneration(ctx, remove); status.Code(err) != codes.Unavailable {
			t.Fatalf("mutation near caller deadline = %v, want Unavailable guidance", err)
		}
	})

	t.Run("reboot and shutdown", func(t *testing.T) {
		bootHealthAdmissionWait = 10 * time.Second
		server, _ := knownGoodNode(t, healthSettling)
		writeRebootEFIDefault(t, server.Server)
		server.RunReboot = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
		server.RunShutdown = func(context.Context, []string, func(int)) ToolResult { return ToolResult{} }
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		if _, err := server.Reboot(ctx, &agentapi.RebootRequest{
			ApiVersion: APIVersion, Kind: RebootRequestKind, Actor: "test", TargetGenerationId: "generation-0",
			ExpectedEnrollmentId: testEnrollmentID, ExpectedInventoryNodeName: testInventoryNodeName,
			ExpectedMachineId: testMachineID, ExpectedCurrentGenerationId: "generation-0",
		}); err != nil {
			t.Fatalf("reboot during boot health: %v", err)
		}
		if _, err := server.Shutdown(ctx, &agentapi.ShutdownRequest{
			ApiVersion: APIVersion, Kind: ShutdownRequestKind, Actor: "test",
			ExpectedEnrollmentId: testEnrollmentID, ExpectedInventoryNodeName: testInventoryNodeName,
			ExpectedMachineId: testMachineID, ExpectedCurrentGenerationId: "generation-0",
		}); err != nil {
			t.Fatalf("shutdown during boot health: %v", err)
		}
	})
}

// Node status reports each link reconfiguration in this boot, whatever its
// outcome, while the node stays healthy.
func TestNodeStatusReportsLinkRecovery(t *testing.T) {
	server, _ := knownGoodNode(t, healthSucceeded)
	server.readManagementNetwork = readReadyManagementNetwork
	dir := filepath.Join(server.Root, "run/katl/link-recovery")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for link, record := range map[string]string{
		"management0": `{"link":"management0","at":"2026-11-27T06:00:00Z","outcome":"reconfigured"}`,
		"management1": `{"link":"management1","at":"2026-11-27T06:01:00Z","outcome":"failed","error":"networkctl reconfigure: exit status 1: access denied"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, link), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	report, err := server.GetNodeStatus(t.Context(), &agentapi.GetNodeStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := report.GetBootHealthDiagnostic()
	if report.GetBootHealthState() != boothealth.Healthy {
		t.Fatalf("boot health = %s (%s), want healthy", report.GetBootHealthState(), diagnostic)
	}
	for _, want := range []string{
		"Required link management0 was reconfigured once in this boot",
		"Required link management1 could not be reconfigured",
		"access denied",
	} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("boot health diagnostic = %q, want %q", diagnostic, want)
		}
	}
	if strings.Contains(diagnostic, ". required") {
		t.Fatalf("boot health diagnostic = %q starts a joined sentence in lowercase", diagnostic)
	}
	if strings.Contains(diagnostic, "management1 was reconfigured") {
		t.Fatalf("boot health diagnostic = %q reports the failed reconfiguration as done", diagnostic)
	}
}
