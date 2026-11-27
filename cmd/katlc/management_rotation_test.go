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

func TestRotateManagementOnNodeRecoversInterruptedReload(t *testing.T) {
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
	failReload := true
	service := func(_ context.Context, action string) error {
		switch action {
		case "reload-or-restart":
			if failReload {
				failReload = false
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
		t.Fatalf("failed reload = %v", err)
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

func TestAgentManagementAuthorityReportsInstalledAuthority(t *testing.T) {
	root := t.TempDir()
	machineID, err := nodeidentity.WriteMachineID(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodeidentity.WriteEnrollment(root, "cp-1", machineID, nil); err != nil {
		t.Fatal(err)
	}
	identity := rotationTestNodeIdentity(t)
	if err := nodeidentity.WriteManagementIdentity(root, "cp-1", identity); err != nil {
		t.Fatal(err)
	}
	want, err := managementidentity.CAFingerprint(identity.CACertificate)
	if err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runAgentManagementAuthority([]string{"--root", root}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var status nodeidentity.ManagementAuthorityStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil || status.Authority != want || status.NodeName != "cp-1" {
		t.Fatalf("status = %+v, %v; want authority %s", status, err, want)
	}
}

func TestRotateManagementOnNodeUnknownExpectedAuthorityOnlyReloads(t *testing.T) {
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
	var actions []string
	service := func(_ context.Context, action string) error {
		actions = append(actions, action)
		return nil
	}
	reloadOnly, err := json.Marshal(nodeidentity.ManagementRotation{
		NodeName: "cp-1", ExpectedMachineID: machineID, ExpectedAuthority: strings.Repeat("0", 64), Replacement: next,
	})
	if err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(root, "var/lib/katl/identity/management/active")

	// With the previous authority still installed, the request cannot install
	// the replacement or reload.
	err = rotateManagementOnNode(t.Context(), root, false, bytes.NewReader(reloadOnly), new(bytes.Buffer), service)
	if err == nil || !strings.Contains(err.Error(), "differs from both") || len(actions) != 0 {
		t.Fatalf("reload-only request on the previous authority = %v, service %v", err, actions)
	}
	if _, err := os.Lstat(active); !os.IsNotExist(err) {
		t.Fatalf("refused request changed active identity: %v", err)
	}

	oldFingerprint, err := managementidentity.CAFingerprint(old.CACertificate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodeidentity.RotateManagementIdentity(root, nodeidentity.ManagementRotation{
		NodeName: "cp-1", ExpectedMachineID: machineID, ExpectedAuthority: oldFingerprint, Replacement: next,
	}); err != nil {
		t.Fatal(err)
	}
	installed, err := os.Lstat(active)
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := rotateManagementOnNode(t.Context(), root, false, bytes.NewReader(reloadOnly), &output, service); err != nil {
		t.Fatalf("reload-only request on the installed replacement: %v", err)
	}
	if strings.Join(actions, " ") != "reload-or-restart is-active" {
		t.Fatalf("service actions = %v, want a reload and an activity check", actions)
	}
	if after, err := os.Lstat(active); err != nil || !os.SameFile(installed, after) {
		t.Fatalf("reload-only request replaced the active identity: %v", err)
	}
	var status nodeidentity.ManagementRotationStatus
	if err := json.Unmarshal(output.Bytes(), &status); err != nil || !status.AlreadyRotated {
		t.Fatalf("status = %s, %v", &output, err)
	}
}
