package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nodeidentity"
)

type fixedTrialClock struct{ sinceBoot time.Duration }

func (fixedTrialClock) BootID() (string, error)             { return "boot-a", nil }
func (c fixedTrialClock) SinceBoot() (time.Duration, error) { return c.sinceBoot, nil }
func (fixedTrialClock) Now() time.Time                      { return time.Now().UTC() }

func TestRevertManagementTrialRestartsAgent(t *testing.T) {
	root := t.TempDir()
	machineID, err := nodeidentity.WriteMachineID(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := nodeidentity.WriteEnrollment(root, "cp-1", machineID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.WriteManagementIdentity(root, "cp-1", managementidentity.NodeCredentials{Authentication: managementidentity.TrustedNetwork}); err != nil {
		t.Fatal(err)
	}
	clock := fixedTrialClock{sinceBoot: time.Hour}
	trial, err := nodeidentity.BeginManagementTrial(root, nodeidentity.ManagementTransition{
		NodeName: "cp-1", ExpectedEnrollmentID: enrollment.ID, ExpectedMachineID: machineID,
		ExpectedAuthentication: managementidentity.TrustedNetwork, Replacement: rotationTestNodeIdentity(t),
	}, clock, func(nodeidentity.ManagementTrial) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var restarts int
	service := func(_ context.Context, action string) error {
		if action != "restart" {
			return errors.New("unexpected action " + action)
		}
		restarts++
		return nil
	}

	var output bytes.Buffer
	if err := revertManagementTrial(t.Context(), root, "", "reverted from the console", clock, &output, service); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 || !strings.Contains(output.String(), trial.ID) {
		t.Fatalf("revert restarted the agent %d times and printed %q", restarts, output.String())
	}
	recorded, found, err := nodeidentity.ReadManagementTrial(root)
	if err != nil || !found || recorded.State != nodeidentity.ManagementTrialReverted || recorded.RevertReason != "reverted from the console" {
		t.Fatalf("trial = %+v, %v, %v", recorded, found, err)
	}

	output.Reset()
	if err := revertManagementTrial(t.Context(), root, "", "again", clock, &output, service); err != nil {
		t.Fatal(err)
	}
	if restarts != 1 || !strings.Contains(output.String(), "no management trial") {
		t.Fatalf("repeated revert restarted the agent %d times and printed %q", restarts, output.String())
	}
}
