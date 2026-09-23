package nodeidentity

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
)

const managementDirectory = "var/lib/katl/identity/management"

type ManagementRotation struct {
	NodeName          string                             `json:"nodeName"`
	ExpectedMachineID string                             `json:"expectedMachineID,omitempty"`
	ExpectedAuthority string                             `json:"expectedAuthority"`
	Replacement       managementidentity.NodeCredentials `json:"replacement"`
}

type ManagementRotationStatus struct {
	NodeName         string `json:"nodeName"`
	MachineID        string `json:"machineID"`
	CurrentAuthority string `json:"currentAuthority"`
	NextAuthority    string `json:"nextAuthority"`
	AlreadyRotated   bool   `json:"alreadyRotated"`
}

// ManagementCredentialsPaths selects one complete credential set. The active
// link changes atomically; a restart therefore reads either the old or new set.
func ManagementCredentialsPaths(root string) (string, string, string, error) {
	dir := filepath.Join(filepath.Clean(root), managementDirectory)
	active := filepath.Join(dir, "active")
	target, err := os.Readlink(active)
	if os.IsNotExist(err) {
		return filepath.Join(dir, "ca.crt"), filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), nil
	}
	if err != nil {
		return "", "", "", fmt.Errorf("read active management identity: %w", err)
	}
	fingerprint := strings.TrimPrefix(target, "versions/")
	if target == fingerprint || len(fingerprint) != 64 {
		return "", "", "", fmt.Errorf("active management identity has invalid target %q", target)
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return "", "", "", fmt.Errorf("active management identity has invalid fingerprint: %w", err)
	}
	base := filepath.Join(dir, target)
	return filepath.Join(base, "ca.crt"), filepath.Join(base, "server.crt"), filepath.Join(base, "server.key"), nil
}

func InspectManagementRotation(root string, request ManagementRotation) (ManagementRotationStatus, error) {
	if strings.TrimSpace(root) == "" {
		return ManagementRotationStatus{}, fmt.Errorf("runtime root is required")
	}
	mode, err := ManagementAuthentication(root)
	if err != nil {
		return ManagementRotationStatus{}, err
	}
	if mode != managementidentity.MutualTLS {
		return ManagementRotationStatus{}, fmt.Errorf("management credentials can rotate only on an mTLS node")
	}
	enrollment, err := ReadEnrollment(root)
	if err != nil {
		return ManagementRotationStatus{}, err
	}
	if enrollment.InventoryNodeName != strings.TrimSpace(request.NodeName) {
		return ManagementRotationStatus{}, fmt.Errorf("node is enrolled as %q, not %q", enrollment.InventoryNodeName, request.NodeName)
	}
	if request.ExpectedMachineID != "" && request.ExpectedMachineID != enrollment.MachineID {
		return ManagementRotationStatus{}, fmt.Errorf("node machine ID changed; expected %q", request.ExpectedMachineID)
	}
	if err := managementidentity.ValidateNode(request.Replacement, enrollment.InventoryNodeName, time.Now().UTC()); err != nil {
		return ManagementRotationStatus{}, fmt.Errorf("validate replacement management identity: %w", err)
	}
	next, err := managementidentity.CAFingerprint(request.Replacement.CACertificate)
	if err != nil {
		return ManagementRotationStatus{}, err
	}
	if len(request.ExpectedAuthority) != 64 {
		return ManagementRotationStatus{}, fmt.Errorf("expected authority fingerprint is required")
	}
	if _, err := hex.DecodeString(request.ExpectedAuthority); err != nil {
		return ManagementRotationStatus{}, fmt.Errorf("expected authority fingerprint is invalid: %w", err)
	}
	if request.ExpectedAuthority == next {
		return ManagementRotationStatus{}, fmt.Errorf("replacement authority must differ from the installed authority")
	}
	caPath, certPath, keyPath, err := ManagementCredentialsPaths(root)
	if err != nil {
		return ManagementRotationStatus{}, err
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return ManagementRotationStatus{}, fmt.Errorf("read installed management authority: %w", err)
	}
	current, err := managementidentity.CAFingerprint(string(ca))
	if err != nil {
		return ManagementRotationStatus{}, fmt.Errorf("installed management authority: %w", err)
	}
	if current != request.ExpectedAuthority && current != next {
		return ManagementRotationStatus{}, fmt.Errorf("installed management authority %s differs from both expected and replacement authorities", current)
	}
	if current == next {
		cert, err := os.ReadFile(certPath)
		if err != nil {
			return ManagementRotationStatus{}, err
		}
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return ManagementRotationStatus{}, err
		}
		if !bytes.Equal(ca, []byte(request.Replacement.CACertificate)) || !bytes.Equal(cert, []byte(request.Replacement.ServerCertificate)) || !bytes.Equal(key, []byte(request.Replacement.ServerPrivateKey)) {
			return ManagementRotationStatus{}, fmt.Errorf("installed credentials differ from replacement material under the same authority")
		}
	}
	return ManagementRotationStatus{
		NodeName: enrollment.InventoryNodeName, MachineID: enrollment.MachineID,
		CurrentAuthority: current, NextAuthority: next, AlreadyRotated: current == next,
	}, nil
}

func RotateManagementIdentity(root string, request ManagementRotation) (ManagementRotationStatus, error) {
	dir := filepath.Join(filepath.Clean(root), managementDirectory)
	lock, err := os.OpenFile(filepath.Join(dir, ".rotation.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return ManagementRotationStatus{}, fmt.Errorf("open management rotation lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return ManagementRotationStatus{}, fmt.Errorf("lock management identity: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	status, err := InspectManagementRotation(root, request)
	if err != nil || status.AlreadyRotated {
		return status, err
	}
	versions := filepath.Join(dir, "versions")
	if err := os.MkdirAll(versions, 0o700); err != nil {
		return ManagementRotationStatus{}, fmt.Errorf("create management identity versions: %w", err)
	}
	if err := stageManagementVersion(versions, status.NextAuthority, request.Replacement); err != nil {
		return ManagementRotationStatus{}, err
	}
	link, err := os.CreateTemp(dir, ".active-")
	if err != nil {
		return ManagementRotationStatus{}, fmt.Errorf("reserve active identity link: %w", err)
	}
	linkPath := link.Name()
	if err := link.Close(); err != nil {
		return ManagementRotationStatus{}, err
	}
	defer os.Remove(linkPath)
	if err := os.Remove(linkPath); err != nil {
		return ManagementRotationStatus{}, err
	}
	if err := os.Symlink(filepath.Join("versions", status.NextAuthority), linkPath); err != nil {
		return ManagementRotationStatus{}, fmt.Errorf("create active identity link: %w", err)
	}
	if err := os.Rename(linkPath, filepath.Join(dir, "active")); err != nil {
		return ManagementRotationStatus{}, fmt.Errorf("switch active management identity: %w", err)
	}
	if err := syncManagementDirectory(dir); err != nil {
		return ManagementRotationStatus{}, err
	}
	status.CurrentAuthority = status.NextAuthority
	status.AlreadyRotated = true
	return status, nil
}

func stageManagementVersion(versions, fingerprint string, identity managementidentity.NodeCredentials) error {
	final := filepath.Join(versions, fingerprint)
	if _, err := os.Stat(final); err == nil {
		return verifyManagementVersion(final, identity)
	} else if !os.IsNotExist(err) {
		return err
	}
	stage, err := os.MkdirTemp(versions, ".stage-")
	if err != nil {
		return fmt.Errorf("stage management identity: %w", err)
	}
	defer os.RemoveAll(stage)
	for _, file := range []struct {
		name string
		data string
		mode os.FileMode
	}{{"ca.crt", identity.CACertificate, 0o444}, {"server.crt", identity.ServerCertificate, 0o444}, {"server.key", identity.ServerPrivateKey, 0o600}} {
		path := filepath.Join(stage, file.name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, file.mode)
		if err != nil {
			return fmt.Errorf("stage %s: %w", file.name, err)
		}
		if _, err := f.WriteString(file.data); err != nil {
			f.Close()
			return fmt.Errorf("write %s: %w", file.name, err)
		}
		if err := f.Chmod(file.mode); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if err := syncManagementDirectory(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, final); err != nil {
		return fmt.Errorf("publish management identity version: %w", err)
	}
	return syncManagementDirectory(versions)
}

func verifyManagementVersion(dir string, identity managementidentity.NodeCredentials) error {
	for _, file := range []struct{ name, data string }{{"ca.crt", identity.CACertificate}, {"server.crt", identity.ServerCertificate}, {"server.key", identity.ServerPrivateKey}} {
		got, err := os.ReadFile(filepath.Join(dir, file.name))
		if err != nil || !bytes.Equal(got, []byte(file.data)) {
			return fmt.Errorf("staged management identity %s differs from replacement material", file.name)
		}
	}
	return nil
}

func syncManagementDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
