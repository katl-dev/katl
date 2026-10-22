package vmtest

import (
	"context"
	"fmt"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
	"github.com/katl-dev/katl/internal/persistedrecord"
)

// FirstInstallRuntimeVMConfig observes completed generation-zero boot through
// systemd and persisted state. Serial output only triggers the observer: kernel
// and userspace writes can interleave on the console.
func FirstInstallRuntimeVMConfig(base VMConfig) VMConfig {
	base.VSock.Enabled = true
	base.Agent.RequireHealth = true
	base.Expect = runtimeKernelBootSignal
	base.SerialHooks = append(append([]SerialHook(nil), base.SerialHooks...), SerialHook{
		Name: "first-install-health", Signal: runtimeKernelBootSignal,
		Run: waitFirstInstallHealth,
	})
	return base
}

func waitFirstInstallHealth(ctx context.Context, event SerialHookEvent) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var guest *GuestControl
	var lastErr error
	for {
		if guest == nil {
			client, err := DialAgent(ctx, event.Plan.VSock.GuestCID, event.Plan.VSock.Port, event.Result.Artifacts.VSockTranscript)
			lastErr = err
			if err == nil {
				defer client.Close()
				guest = NewGuestControl(event.Result, client)
			}
		}
		if guest != nil {
			if err := guest.WaitBootComplete(ctx); err != nil {
				return err
			}
			return checkFirstInstallHealth(ctx, guest)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for first-install boot completion: %w (last observation: %v)", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func checkFirstInstallHealth(ctx context.Context, guest *GuestControl) error {
	if err := checkBootHealth(ctx, guest); err != nil {
		return err
	}

	file, err := guest.ReadFile(ctx, GuestFileRequest{Path: "/var/lib/katl/install/status.json", StoreContent: true})
	if err != nil {
		return err
	}
	if file.Truncated {
		return fmt.Errorf("truncated first-install status")
	}
	status, err := installstatus.ReadFile(file.Artifact)
	if err != nil {
		return err
	}
	if status.FinalHandoff != installstatus.StateWaitingForClusterBootstrap || status.LastError != "" {
		return fmt.Errorf("first-install handoff = %q, last error = %q", status.FinalHandoff, status.LastError)
	}

	file, err = guest.ReadFile(ctx, GuestFileRequest{Path: "/var/lib/katl/boot/selection.json", StoreContent: true})
	if err != nil {
		return err
	}
	if file.Truncated {
		return fmt.Errorf("truncated first-install boot selection")
	}
	envelope, err := persistedrecord.ReadEnvelope(file.Artifact)
	if err != nil {
		return err
	}
	if envelope.RecordType != generation.BootSelectionRecordType || envelope.RecordVersion != 1 {
		return fmt.Errorf("unexpected boot-selection record %s/v%d", envelope.RecordType, envelope.RecordVersion)
	}
	selection, err := persistedrecord.DecodePayload[generation.BootSelectionRecord](envelope)
	if err != nil {
		return err
	}
	if err := generation.ValidateBootSelection(selection); err != nil {
		return err
	}
	if selection.DefaultGenerationID != "0" || selection.BootedGenerationID != "0" || selection.PendingHealthValidation || selection.RecoveryRequired || selection.PersistentDefaultPromotion != generation.DefaultPromotionDone {
		return fmt.Errorf("first-install generation zero is not healthy and promoted: %+v", selection)
	}
	return nil
}
