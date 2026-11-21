package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
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

// healthUnit fakes systemd's view of katl-boot-health.service.
type healthUnit struct{ properties atomic.Value }

func (u *healthUnit) set(properties string) { u.properties.Store(properties) }

func (u *healthUnit) run(_ context.Context, argv []string, _ func(int)) ToolResult {
	return ToolResult{Stdout: []byte(u.properties.Load().(string))}
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
	writeProcCmdline(t, root, "root=PARTUUID=11111111-1111-1111-1111-111111111111 rootfstype=squashfs ro katl.generation=generation-0")
	unit := &healthUnit{}
	unit.set(unitProperties)
	server.RunBootHealthStatus = unit.run
	return server, unit
}

func TestNodeStatusBootHealthFollowsCurrentBoot(t *testing.T) {
	for _, tc := range []struct {
		name, unit, want string
	}{
		{name: "settling", unit: healthSettling, want: nodeBootHealthPending},
		{name: "succeeded", unit: healthSucceeded, want: nodeBootHealthHealthy},
		{name: "failed", unit: healthFailed, want: nodeBootHealthFailed},
		{name: "dependency failed", unit: healthDependencyFailed, want: nodeBootHealthFailed},
		{name: "settle timed out", unit: healthStalled, want: nodeBootHealthPending},
		{name: "units unavailable", unit: healthUnitsMissing, want: nodeBootHealthHealthy},
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
