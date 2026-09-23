package nodeidentity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/managementidentity"
)

func TestManagementRotationSwitchesCompleteIdentityAndResumes(t *testing.T) {
	root := t.TempDir()
	machineID, err := WriteMachineID(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteEnrollment(root, "cp-1", machineID, nil); err != nil {
		t.Fatal(err)
	}
	old := testManagementIdentity(t, "cp-1")
	next := testManagementIdentity(t, "cp-1")
	if err := WriteManagementIdentity(root, "cp-1", old); err != nil {
		t.Fatal(err)
	}
	oldFingerprint, err := managementidentity.CAFingerprint(old.CACertificate)
	if err != nil {
		t.Fatal(err)
	}
	nextFingerprint, err := managementidentity.CAFingerprint(next.CACertificate)
	if err != nil {
		t.Fatal(err)
	}
	request := ManagementRotation{NodeName: "cp-1", ExpectedMachineID: machineID, ExpectedAuthority: oldFingerprint, Replacement: next}

	before, err := InspectManagementRotation(root, request)
	if err != nil || before.AlreadyRotated || before.CurrentAuthority != oldFingerprint {
		t.Fatalf("preflight = %+v, %v", before, err)
	}
	if _, err := InspectManagementRotation(root, ManagementRotation{NodeName: "cp-1", ExpectedAuthority: strings.Repeat("a", 64), Replacement: next}); err == nil || !strings.Contains(err.Error(), "differs from both") {
		t.Fatalf("wrong expected authority was accepted: %v", err)
	}
	for range 2 {
		status, err := RotateManagementIdentity(root, request)
		if err != nil || !status.AlreadyRotated || status.CurrentAuthority != nextFingerprint {
			t.Fatalf("rotate = %+v, %v", status, err)
		}
	}
	caPath, certPath, keyPath, err := ManagementCredentialsPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{caPath: next.CACertificate, certPath: next.ServerCertificate, keyPath: next.ServerPrivateKey} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("active credential %s does not match replacement: %v", filepath.Base(path), err)
		}
	}
	assertMode(t, keyPath, 0o600)
	legacy, err := os.ReadFile(filepath.Join(root, ManagementCACertificatePath))
	if err != nil || string(legacy) != old.CACertificate {
		t.Fatalf("recovery identity changed: %v", err)
	}
	if err := WriteManagementIdentity(root, "cp-1", next); err != nil {
		t.Fatalf("reinstall under rotated authority: %v", err)
	}
	if err := WriteManagementIdentity(root, "cp-1", old); err == nil {
		t.Fatal("reinstall reopened exposed authority")
	}
}

func TestManagementRotationRejectsOtherNode(t *testing.T) {
	root := t.TempDir()
	machineID, err := WriteMachineID(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteEnrollment(root, "cp-1", machineID, nil); err != nil {
		t.Fatal(err)
	}
	old := testManagementIdentity(t, "cp-1")
	if err := WriteManagementIdentity(root, "cp-1", old); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := managementidentity.CAFingerprint(old.CACertificate)
	if err != nil {
		t.Fatal(err)
	}
	request := ManagementRotation{NodeName: "cp-2", ExpectedAuthority: fingerprint, Replacement: testManagementIdentity(t, "cp-2")}
	if _, err := RotateManagementIdentity(root, request); err == nil || !strings.Contains(err.Error(), "enrolled") {
		t.Fatalf("wrong node was accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, managementDirectory, "active")); !os.IsNotExist(err) {
		t.Fatalf("wrong-node request changed active credentials: %v", err)
	}
}
