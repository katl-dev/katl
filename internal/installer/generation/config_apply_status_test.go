package generation

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigApplyStatusSerializesSchema(t *testing.T) {
	status, err := NewConfigApplyStatus(ConfigApplyStatusRequest{
		GenerationID:       "2026.06.05-002",
		PreviousGeneration: "2026.06.05-001",
		RequestedApplyMode: ApplyModeLive,
		AcceptedApplyMode:  ApplyModeLive,
		ChangedDomains:     []string{"host-configuration", "host-configuration", "tmpfiles"},
		HealthState:        "unknown",
		Kubeadm: KubeadmActionRequired{
			Required:           true,
			PreviousConfigName: "control-plane-old",
			SelectedConfigName: "control-plane",
			Reason:             "rendered kubeadm input differs; join token abcdef.0123456789abcdef requires explicit action",
		},
		UpdatedAt: time.Date(2026, 6, 5, 15, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewConfigApplyStatus() error = %v", err)
	}
	status.DomainActions = []ConfigApplyDomainAction{{
		Domain: "host-configuration",
		Action: "stage-next-boot",
		Status: ConfigApplyActionPlanned,
	}}
	status.DiagnosticArtifacts = []DiagnosticArtifact{{
		Name: "planner",
		Path: "/var/lib/katl/generations/2026.06.05-002/config-apply-plan.json",
	}}

	data, err := MarshalConfigApplyStatus(status)
	if err != nil {
		t.Fatalf("MarshalConfigApplyStatus() error = %v", err)
	}
	want := `{
  "recordType": "katl.generation.config-apply-status",
  "recordVersion": 1,
  "payload": {
    "apiVersion": "katl.dev/v1alpha1",
    "kind": "ConfigApplyStatus",
    "generationID": "2026.06.05-002",
    "previousGenerationID": "2026.06.05-001",
    "requestedApplyMode": "live",
    "acceptedApplyMode": "live",
    "changedDomains": [
      "host-configuration",
      "tmpfiles"
    ],
    "phase": "planned",
    "healthState": "unknown",
    "domainActions": [
      {
        "domain": "host-configuration",
        "action": "stage-next-boot",
        "status": "planned"
      }
    ],
    "diagnosticArtifacts": [
      {
        "name": "planner",
        "path": "/var/lib/katl/generations/2026.06.05-002/config-apply-plan.json"
      }
    ],
    "kubeadm": {
      "required": true,
      "previousConfigName": "control-plane-old",
      "selectedConfigName": "control-plane",
      "reason": "rendered kubeadm input differs; join token [REDACTED BOOTSTRAP TOKEN] requires explicit action"
    },
    "updatedAt": "2026-06-05T15:30:00Z"
  }
}
`
	if string(data) != want {
		t.Fatalf("status json:\n%s\nwant:\n%s", data, want)
	}
}

func TestConfigApplyStatusPathIsGenerationLocalSibling(t *testing.T) {
	root := t.TempDir()
	statusPath, err := ConfigApplyStatusPath(root, "2026.06.05-002")
	if err != nil {
		t.Fatalf("ConfigApplyStatusPath() error = %v", err)
	}
	metadataPath, err := MetadataPath(root, "2026.06.05-002")
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	if statusPath != filepath.Join(root, "var/lib/katl/generations/2026.06.05-002/config-apply-status.json") {
		t.Fatalf("status path = %q", statusPath)
	}
	if filepath.Dir(statusPath) != filepath.Dir(metadataPath) || filepath.Base(statusPath) == filepath.Base(metadataPath) {
		t.Fatalf("status path %q is not a sibling of metadata path %q", statusPath, metadataPath)
	}
}

func TestWriteConfigApplyStatusDoesNotMutateMetadata(t *testing.T) {
	root := t.TempDir()
	record := abRecord(t, "2026.06.05-002", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.36.1", time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC))
	metadataPath, err := MetadataPath(root, record.GenerationID)
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	if err := WriteRecord(metadataPath, record); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}
	before, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata before: %v", err)
	}

	status := validConfigApplyStatus(t)
	statusPath, err := ConfigApplyStatusPath(root, status.GenerationID)
	if err != nil {
		t.Fatalf("ConfigApplyStatusPath() error = %v", err)
	}
	if err := WriteConfigApplyStatus(statusPath, status); err != nil {
		t.Fatalf("WriteConfigApplyStatus() error = %v", err)
	}
	after, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("read metadata after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("metadata changed after writing status:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	decoded, err := ReadConfigApplyStatus(statusPath)
	if err != nil {
		t.Fatalf("ReadConfigApplyStatus() error = %v", err)
	}
	if decoded.GenerationID != status.GenerationID || decoded.Phase != ConfigApplyPhasePlanned {
		t.Fatalf("decoded status = %#v", decoded)
	}
}

func TestConfigApplyStatusPhaseTransitions(t *testing.T) {
	status := validConfigApplyStatus(t)
	now := time.Date(2026, 6, 5, 16, 0, 0, 0, time.UTC)

	status, err := MarkConfigApplyPhase(status, ConfigApplyPhaseActivating, now)
	if err != nil {
		t.Fatalf("MarkConfigApplyPhase() error = %v", err)
	}
	if status.Phase != ConfigApplyPhaseActivating || !status.UpdatedAt.Equal(now) {
		t.Fatalf("status = %#v", status)
	}
	status, err = MarkConfigApplyPhase(status, ConfigApplyPhaseActive, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("MarkConfigApplyPhase(active) error = %v", err)
	}
	if status.Phase != ConfigApplyPhaseActive || !status.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("status = %#v", status)
	}
	if _, err := MarkConfigApplyPhase(status, "half-live", now); err == nil {
		t.Fatal("MarkConfigApplyPhase() error = nil, want invalid phase")
	}
}

func TestConfigApplyStatusRedactsFailureReason(t *testing.T) {
	status := validConfigApplyStatus(t)
	cause := errors.New("apply failed with https://user:pass@example.invalid/path?token=secret and Bearer abc.def.ghi and abcdef.0123456789abcdef")

	failed, err := MarkConfigApplyFailed(status, cause, time.Date(2026, 6, 5, 16, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("MarkConfigApplyFailed() error = %v", err)
	}
	if failed.Phase != ConfigApplyPhaseFailed {
		t.Fatalf("phase = %q", failed.Phase)
	}
	for _, leaked := range []string{"user:pass", "token=secret", "abc.def.ghi", "abcdef.0123456789abcdef"} {
		if strings.Contains(failed.FailureReason, leaked) {
			t.Fatalf("failure reason leaked %q in %q", leaked, failed.FailureReason)
		}
	}
	for _, want := range []string{"https://example.invalid/path", "Bearer [REDACTED]", "[REDACTED BOOTSTRAP TOKEN]"} {
		if !strings.Contains(failed.FailureReason, want) {
			t.Fatalf("failure reason = %q, missing %q", failed.FailureReason, want)
		}
	}
}

func TestConfigApplyRollbackFields(t *testing.T) {
	status := validConfigApplyStatus(t)
	rolledBack, err := MarkConfigApplyRollback(status, "2026.06.05-001", "passed", "restored after token abcdef.0123456789abcdef", time.Date(2026, 6, 5, 16, 10, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("MarkConfigApplyRollback() error = %v", err)
	}
	if rolledBack.Phase != ConfigApplyPhaseRolledBack || rolledBack.Rollback == nil {
		t.Fatalf("rollback status = %#v", rolledBack)
	}
	if rolledBack.Rollback.TargetGenerationID != "2026.06.05-001" || rolledBack.Rollback.Result != "passed" {
		t.Fatalf("rollback = %#v", rolledBack.Rollback)
	}
	if strings.Contains(rolledBack.Rollback.Reason, "abcdef.0123456789abcdef") || !strings.Contains(rolledBack.Rollback.Reason, "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("rollback reason was not redacted: %q", rolledBack.Rollback.Reason)
	}
}

func TestConfigApplyStatusRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(ConfigApplyStatus) ConfigApplyStatus
		wantErr string
	}{
		{
			name: "metadata kind",
			mutate: func(status ConfigApplyStatus) ConfigApplyStatus {
				status.Kind = Kind
				return status
			},
			wantErr: "kind",
		},
		{
			name: "apply mode",
			mutate: func(status ConfigApplyStatus) ConfigApplyStatus {
				status.RequestedApplyMode = "immediate"
				return status
			},
			wantErr: "requested apply mode",
		},
		{
			name: "changed domains",
			mutate: func(status ConfigApplyStatus) ConfigApplyStatus {
				status.ChangedDomains = nil
				return status
			},
			wantErr: "changed domains",
		},
		{
			name: "action status",
			mutate: func(status ConfigApplyStatus) ConfigApplyStatus {
				status.DomainActions = []ConfigApplyDomainAction{{Domain: "host-configuration", Action: "stage-next-boot", Status: "maybe"}}
				return status
			},
			wantErr: "action status",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateConfigApplyStatus(tt.mutate(validConfigApplyStatus(t)))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateConfigApplyStatus() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigApplyStatusRoundTripJSON(t *testing.T) {
	status := validConfigApplyStatus(t)
	status.DomainActions = []ConfigApplyDomainAction{{Domain: "tmpfiles", Action: "systemd-tmpfiles", Status: ConfigApplyActionPassed}}
	status.DiagnosticArtifacts = []DiagnosticArtifact{{Name: "actions", Path: "/var/lib/katl/generations/2026.06.05-002/actions.json"}}

	data, err := MarshalConfigApplyStatus(status)
	if err != nil {
		t.Fatalf("MarshalConfigApplyStatus() error = %v", err)
	}
	decoded, err := decodeConfigApplyStatus(data)
	if err != nil {
		t.Fatalf("decodeConfigApplyStatus() error = %v", err)
	}
	if err := ValidateConfigApplyStatus(decoded); err != nil {
		t.Fatalf("ValidateConfigApplyStatus(decoded) error = %v", err)
	}
	if decoded.GenerationID != status.GenerationID || decoded.DomainActions[0].Status != ConfigApplyActionPassed {
		t.Fatalf("decoded = %#v, want %#v", decoded, status)
	}
}

func validConfigApplyStatus(t *testing.T) ConfigApplyStatus {
	t.Helper()
	status, err := NewConfigApplyStatus(ConfigApplyStatusRequest{
		GenerationID:       "2026.06.05-002",
		PreviousGeneration: "2026.06.05-001",
		RequestedApplyMode: ApplyModeNextBoot,
		AcceptedApplyMode:  ApplyModeNextBoot,
		ChangedDomains:     []string{"kubeadm-config"},
		HealthState:        "unknown",
		UpdatedAt:          time.Date(2026, 6, 5, 15, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewConfigApplyStatus() error = %v", err)
	}
	return status
}
