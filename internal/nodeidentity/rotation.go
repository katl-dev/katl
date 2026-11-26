package nodeidentity

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
)

const managementDirectory = "var/lib/katl/identity/management"

// managementLockTimeout bounds the wait for the management identity lock.
var managementLockTimeout = 30 * time.Second

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

// ManagementAuthorityStatus is the installed management authority of an mTLS
// node, read so an operator without the original secrets can replace it.
type ManagementAuthorityStatus struct {
	NodeName  string `json:"nodeName"`
	MachineID string `json:"machineID"`
	Authority string `json:"authority"`
}

// InspectManagementAuthority reports the fingerprint of the CA the node
// trusts. It changes nothing and refuses a node that is not mTLS.
func InspectManagementAuthority(root string) (ManagementAuthorityStatus, error) {
	if strings.TrimSpace(root) == "" {
		return ManagementAuthorityStatus{}, fmt.Errorf("runtime root is required")
	}
	mode, err := ManagementAuthentication(root)
	if err != nil {
		return ManagementAuthorityStatus{}, err
	}
	if mode != managementidentity.MutualTLS {
		return ManagementAuthorityStatus{}, fmt.Errorf("node uses %s management and has no management authority to recover", mode)
	}
	enrollment, err := ReadEnrollment(root)
	if err != nil {
		return ManagementAuthorityStatus{}, err
	}
	caPath, _, _, err := ManagementCredentialsPaths(root)
	if err != nil {
		return ManagementAuthorityStatus{}, err
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return ManagementAuthorityStatus{}, fmt.Errorf("read installed management authority: %w", err)
	}
	fingerprint, err := managementidentity.CAFingerprint(string(ca))
	if err != nil {
		return ManagementAuthorityStatus{}, fmt.Errorf("installed management authority: %w", err)
	}
	return ManagementAuthorityStatus{NodeName: enrollment.InventoryNodeName, MachineID: enrollment.MachineID, Authority: fingerprint}, nil
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
	ca, cert, key := ManagementVersionPaths(root, fingerprint)
	return ca, cert, key, nil
}

// ManagementVersionPaths names the credential files staged for one authority.
func ManagementVersionPaths(root, fingerprint string) (string, string, string) {
	base := filepath.Join(filepath.Clean(root), managementDirectory, "versions", fingerprint)
	return filepath.Join(base, "ca.crt"), filepath.Join(base, "server.crt"), filepath.Join(base, "server.key")
}

// errManagementTrialInProgress refuses a rotation while a trial could still
// overwrite or discard the active link it would switch.
var errManagementTrialInProgress = errors.New("a management trust transition is in progress; wait for it to confirm or revert, or run `katlc agent management-trial revert`")

func InspectManagementRotation(root string, request ManagementRotation) (ManagementRotationStatus, error) {
	if strings.TrimSpace(root) == "" {
		return ManagementRotationStatus{}, fmt.Errorf("runtime root is required")
	}
	unlock, err := lockManagementIdentity(filepath.Join(filepath.Clean(root), managementDirectory))
	if err != nil {
		return ManagementRotationStatus{}, err
	}
	defer unlock()
	return inspectManagementRotationLocked(root, request)
}

func inspectManagementRotationLocked(root string, request ManagementRotation) (ManagementRotationStatus, error) {
	trial, found, err := ReadManagementTrial(root)
	if err != nil {
		return ManagementRotationStatus{}, err
	}
	if found && trial.State == ManagementTrialLive {
		return ManagementRotationStatus{}, errManagementTrialInProgress
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
	// When expected and replacement match, only the already-installed
	// credentials qualify, so a recovery retry can reload without switching.
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
	unlock, err := lockManagementIdentity(dir)
	if err != nil {
		return ManagementRotationStatus{}, err
	}
	defer unlock()

	status, err := inspectManagementRotationLocked(root, request)
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
	if err := publishActiveManagementVersion(dir, filepath.Join("versions", status.NextAuthority)); err != nil {
		return ManagementRotationStatus{}, err
	}
	status.CurrentAuthority = status.NextAuthority
	status.AlreadyRotated = true
	return status, nil
}

// lockManagementIdentity serializes every change to the installed management
// credentials, mode and trial record across the agent and SSH or timer commands.
func lockManagementIdentity(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create management identity directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".rotation.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open management identity lock: %w", err)
	}
	// Holders only change a few files, so a holder that keeps the lock this
	// long is stuck; fail rather than stall a revert behind it.
	deadline := time.Now().Add(managementLockTimeout)
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			lock.Close()
			return nil, fmt.Errorf("lock management identity: %w", err)
		}
		if time.Now().After(deadline) {
			lock.Close()
			return nil, fmt.Errorf("another katlc process has held the management identity lock for over %s; inspect katlc-agent and running katlc commands on the node, then retry", managementLockTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}, nil
}

// publishActiveManagementVersion atomically points active at target, or
// removes it when target is empty so the legacy credential files apply again.
func publishActiveManagementVersion(dir, target string) error {
	active := filepath.Join(dir, "active")
	if target == "" {
		if err := os.Remove(active); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove active management identity: %w", err)
		}
		return syncManagementDirectory(dir)
	}
	link, err := os.CreateTemp(dir, ".active-")
	if err != nil {
		return fmt.Errorf("reserve active identity link: %w", err)
	}
	linkPath := link.Name()
	if err := link.Close(); err != nil {
		return err
	}
	defer os.Remove(linkPath)
	if err := os.Remove(linkPath); err != nil {
		return err
	}
	if err := os.Symlink(target, linkPath); err != nil {
		return fmt.Errorf("create active identity link: %w", err)
	}
	if err := os.Rename(linkPath, active); err != nil {
		return fmt.Errorf("switch active management identity: %w", err)
	}
	return syncManagementDirectory(dir)
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
