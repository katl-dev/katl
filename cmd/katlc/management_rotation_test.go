package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/nodeidentity"
)

func TestRotateManagementOnNodeRecoversInterruptedRestart(t *testing.T) {
	root := t.TempDir()
	machineID, err := nodeidentity.WriteMachineID(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodeidentity.WriteEnrollment(root, "cp-1", machineID, nil); err != nil {
		t.Fatal(err)
	}
	old := rotationTestNodeIdentity(t)
	next := rotationTestNodeIdentity(t)
	if err := nodeidentity.WriteManagementIdentity(root, "cp-1", old); err != nil {
		t.Fatal(err)
	}
	oldFingerprint, err := managementidentity.CAFingerprint(old.CACertificate)
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(nodeidentity.ManagementRotation{NodeName: "cp-1", ExpectedMachineID: machineID, ExpectedAuthority: oldFingerprint, Replacement: next})
	if err != nil {
		t.Fatal(err)
	}
	running := true
	failRestart := true
	service := func(_ context.Context, action string) error {
		switch action {
		case "restart":
			if failRestart {
				failRestart = false
				return errors.New("startup failed")
			}
			running = true
		case "is-active":
			if !running {
				return errors.New("inactive")
			}
		}
		return nil
	}
	var output bytes.Buffer
	if err := rotateManagementOnNode(t.Context(), root, true, bytes.NewReader(request), &output, service); err != nil {
		t.Fatal(err)
	}
	if !running {
		t.Fatal("dry run interrupted the agent")
	}
	if _, err := os.Lstat(filepath.Join(root, "var/lib/katl/identity/management/active")); !os.IsNotExist(err) {
		t.Fatalf("dry run changed active identity: %v", err)
	}
	output.Reset()
	err = rotateManagementOnNode(t.Context(), root, false, bytes.NewReader(request), &output, service)
	if err == nil || !strings.Contains(err.Error(), "startup failed") {
		t.Fatalf("failed restart = %v", err)
	}
	if err := rotateManagementOnNode(t.Context(), root, false, bytes.NewReader(request), &output, service); err != nil {
		t.Fatalf("resume rotation: %v", err)
	}
	if !running {
		t.Fatal("agent was not running after resume")
	}
	caPath, _, _, err := nodeidentity.ManagementCredentialsPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(caPath)
	if err != nil || string(ca) != next.CACertificate {
		t.Fatalf("active authority after resume: %v", err)
	}
}

func rotationTestNodeIdentity(t *testing.T) managementidentity.NodeCredentials {
	t.Helper()
	bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "lab", Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return mustIssueRotationNode(t, bundle)
}

func mustIssueRotationNode(t *testing.T, bundle managementidentity.Bundle) managementidentity.NodeCredentials {
	t.Helper()
	identity, err := managementidentity.IssueNode(bundle, "cp-1", time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
