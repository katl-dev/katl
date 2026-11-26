package vmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/persistedrecord"
)

func bootSelectionFromGuest(t *testing.T, ctx context.Context, guest *GuestControl) generation.BootSelectionRecord {
	t.Helper()
	data := readGuestFile(t, ctx, guest, "/var/lib/katl/boot/selection.json")
	selection, err := decodeGuestRecord[generation.BootSelectionRecord]([]byte(data), generation.BootSelectionRecordType)
	if err != nil {
		t.Fatalf("decode boot selection: %v\n%s", err, data)
	}
	if err := generation.ValidateBootSelection(selection); err != nil {
		t.Fatalf("guest boot selection is invalid: %v\n%s", err, data)
	}
	return selection
}

func generationRecordsFromGuest(t *testing.T, ctx context.Context, guest *GuestControl, generationID string) (generation.GenerationSpec, generation.GenerationStatus) {
	t.Helper()
	specData := readGuestFile(t, ctx, guest, "/var/lib/katl/generations/"+generationID+"/spec.json")
	statusData := readGuestFile(t, ctx, guest, "/var/lib/katl/generations/"+generationID+"/status.json")
	spec, err := decodeGuestRecord[generation.GenerationSpec]([]byte(specData), generation.GenerationSpecRecordType)
	if err != nil {
		t.Fatalf("decode candidate generation spec: %v\n%s", err, specData)
	}
	status, err := decodeGuestRecord[generation.GenerationStatus]([]byte(statusData), generation.GenerationStatusRecordType)
	if err != nil {
		t.Fatalf("decode candidate generation status: %v\n%s", err, statusData)
	}
	if err := generation.ValidateGenerationStatus(spec, status); err != nil {
		t.Fatalf("candidate generation invalid: %v\nspec=%s\nstatus=%s", err, specData, statusData)
	}
	return spec, status
}

func waitGenerationPromotion(t *testing.T, ctx context.Context, guest *GuestControl, generationID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	// Persisted selection can already name the generation before this boot's
	// health runs, as after a persistent rollback.
	bootCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := guest.WaitBootComplete(bootCtx); err != nil {
		t.Fatalf("generation %s boot did not complete: %v", generationID, err)
	}
	var lastStatus generation.GenerationStatus
	var lastSelection generation.BootSelectionRecord
	for time.Now().Before(deadline) {
		_, lastStatus = generationRecordsFromGuest(t, ctx, guest, generationID)
		lastSelection = bootSelectionFromGuest(t, ctx, guest)
		if lastStatus.CommitState == generation.CommitStateCommitted &&
			lastStatus.BootState == generation.BootStateGood &&
			lastStatus.HealthState == generation.HealthStateHealthy &&
			lastSelection.DefaultGenerationID == generationID &&
			!lastSelection.PendingHealthValidation {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("generation %s was not promoted; status=%#v selection=%#v", generationID, lastStatus, lastSelection)
}

func decodeGuestRecord[T any](data []byte, recordType string) (T, error) {
	var value T
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return value, err
	}
	if _, enveloped := fields["recordType"]; !enveloped {
		return value, json.Unmarshal(data, &value)
	}
	envelope, err := persistedrecord.DecodeEnvelope(data)
	if err != nil {
		return value, err
	}
	if envelope.RecordType != recordType || envelope.RecordVersion != 1 {
		return value, fmt.Errorf("got %s/v%d, want %s/v1", envelope.RecordType, envelope.RecordVersion, recordType)
	}
	return persistedrecord.DecodePayload[T](envelope)
}
