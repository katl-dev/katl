package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestConfigurationMutationRefusedDuringPendingBoot(t *testing.T) {
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error { return nil })
	selection, err := generation.ReadBootSelection(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	selection.TargetBootGenerationID = "upgrade-target"
	selection.TargetBootEntry = "loader/entries/katl-upgrade-target.conf"
	selection.TrialGenerationID = "upgrade-target"
	selection.TrialBootEntry = selection.TargetBootEntry
	selection.PendingHealthValidation = true
	selection.PendingTransactionID = "upgrade-operation"
	selection.PersistentDefaultPromotion = generation.DefaultPromotionPending
	if err := generation.WriteBootSelection(server.Root, selection); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"live", "next-boot"} {
		t.Run(mode, func(t *testing.T) {
			req := &agentapi.GenerationApplyRequest{
				ApiVersion: APIVersion, Kind: "GenerationApplyRequest", ClientRequestId: mode,
				Actor:                 "test",
				CandidateGenerationId: "config-" + mode, ConfigYaml: configApplyYAML(mode),
			}
			if mode == "live" {
				_, err = server.ApplyGeneration(context.Background(), req)
			} else {
				_, err = server.StageGeneration(context.Background(), req)
			}
			if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "boot trial") {
				t.Fatalf("pending boot apply error = %v", err)
			}
		})
	}
	ids, err := server.Store.OperationIDs()
	if err != nil || len(ids) != 0 {
		t.Fatalf("rejected configuration operations = %v, %v", ids, err)
	}
}

func TestInterruptedConfigRenderReleasesLockAndCandidate(t *testing.T) {
	server, id, candidateDir := interruptedConfigRenderFixture(t)
	if _, err := AuditStartup(server.Store, server.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := server.Store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if before.Terminal || len(before.ResourceLocks) == 0 {
		t.Fatalf("startup audit did not reproduce held lock: %+v", before)
	}

	if err := recoverInterruptedConfigApplies(server.Root, server.Store, server.Now()); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedConfigApplies(server.Root, server.Store, server.Now()); err != nil {
		t.Fatalf("repeated recovery: %v", err)
	}
	if _, err := os.Stat(candidateDir); !os.IsNotExist(err) {
		t.Fatalf("partial candidate remains: %v", err)
	}
	result, err := server.GetOperation(context.Background(), &agentapi.GetOperationRequest{OperationId: id})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Terminal || result.Result != "failed" || result.RecoveryRequired || !strings.Contains(result.NextAction, "submit a new configuration apply") {
		t.Fatalf("recovered operation = %+v", result)
	}
	if _, err := server.StageGeneration(context.Background(), &agentapi.GenerationApplyRequest{
		ApiVersion: APIVersion, Kind: "GenerationApplyRequest", ClientRequestId: "retry-after-recovery",
		Actor:                 "test",
		CandidateGenerationId: "config-retry", ConfigYaml: configApplyYAML(generation.ApplyModeNextBoot),
	}); err != nil {
		t.Fatalf("public retry remained blocked: %v", err)
	}
}

func TestInterruptedConfigRenderKeepsSelectedOrMutatedCandidate(t *testing.T) {
	for _, scenario := range []string{"selected", "live mutation", "unreadable published record"} {
		t.Run(scenario, func(t *testing.T) {
			server, id, candidateDir := interruptedConfigRenderFixture(t)
			if scenario == "selected" {
				selection, err := generation.ReadBootSelection(server.Root)
				if err != nil {
					t.Fatal(err)
				}
				selection.TargetBootGenerationID = "config-interrupted"
				selection.TargetBootEntry = "loader/entries/katl-config-interrupted.conf"
				if err := generation.WriteBootSelection(server.Root, selection); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "live mutation" {
				if _, err := server.Store.Update(id, "mutation-started", "render-generation", func(record operation.OperationRecord) (operation.OperationRecord, error) {
					record.ExternalMutationStarted = true
					return record, nil
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(candidateDir, "status.json"), []byte("invalid"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := AuditStartup(server.Store, server.Now()); err != nil {
				t.Fatal(err)
			}
			if err := recoverInterruptedConfigApplies(server.Root, server.Store, server.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(candidateDir); err != nil {
				t.Fatalf("unsafe candidate was removed: %v", err)
			}
			record, err := server.Store.Read(id)
			if err != nil {
				t.Fatal(err)
			}
			if record.Terminal || scenario == "live mutation" && !record.RecoveryRequired {
				t.Fatalf("unsafe operation was cleared: %+v", record)
			}
		})
	}
}

func interruptedConfigRenderFixture(t *testing.T) (*testServer, string, string) {
	t.Helper()
	server := newTestServer(t)
	writeConfigApplyBaseState(t, server.Root)
	server.Dispatcher = dispatchFunc(func(context.Context, operation.OperationRecord) error { return nil })
	accepted, err := server.StageGeneration(context.Background(), &agentapi.GenerationApplyRequest{
		ApiVersion: APIVersion, Kind: "GenerationApplyRequest", ClientRequestId: "interrupted",
		Actor:                 "test",
		CandidateGenerationId: "config-interrupted", ConfigYaml: configApplyYAML(generation.ApplyModeNextBoot),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Store.Update(accepted.OperationId, "render-generation-start", "render-generation", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "render-generation"
		return record, nil
	}); err != nil {
		t.Fatal(err)
	}
	dir, err := generation.GenerationDir(server.Root, "config-interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "confext"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "confext", "partial"), []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	return server, accepted.OperationId, dir
}
