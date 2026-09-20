package nodeidentity

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
)

const (
	EnrollmentPath                 = "var/lib/katl/identity/enrollment.json"
	ManagementAuthenticationPath   = "var/lib/katl/identity/management/authentication"
	ManagementCACertificatePath    = "var/lib/katl/identity/management/ca.crt"
	ManagementServerCertPath       = "var/lib/katl/identity/management/server.crt"
	ManagementServerPrivateKeyPath = "var/lib/katl/identity/management/server.key"
)

type Enrollment struct {
	APIVersion        string `json:"apiVersion"`
	Kind              string `json:"kind"`
	ID                string `json:"id"`
	InventoryNodeName string `json:"inventoryNodeName"`
	MachineID         string `json:"machineID"`
}

type IdentityRequest struct {
	AuthorizedKeys    []string
	InventoryNodeName string
	Management        managementidentity.NodeCredentials
	Random            io.Reader
	EnrollmentRandom  io.Reader
}

type IdentityAssets struct {
	MachineID      string
	Enrollment     Enrollment
	AuthorizedKeys string
	Management     managementidentity.NodeCredentials
}

func RenderSSH(keys []string) (IdentityAssets, error) {
	cleaned, err := cleanKeys(keys)
	if err != nil {
		return IdentityAssets{}, err
	}
	return IdentityAssets{
		AuthorizedKeys: strings.Join(append(cleaned, ""), "\n"),
	}, nil
}

func WriteIdentity(root string, request IdentityRequest) (IdentityAssets, error) {
	if strings.TrimSpace(root) == "" {
		return IdentityAssets{}, fmt.Errorf("target root is required")
	}
	machineID, err := WriteMachineID(root, request.Random)
	if err != nil {
		return IdentityAssets{}, err
	}
	assets, err := RenderSSH(request.AuthorizedKeys)
	if err != nil {
		return IdentityAssets{}, err
	}
	assets.MachineID = machineID
	assets.Enrollment, err = WriteEnrollment(root, request.InventoryNodeName, machineID, request.EnrollmentRandom)
	if err != nil {
		return IdentityAssets{}, err
	}
	if err := WriteManagementIdentity(root, request.InventoryNodeName, request.Management); err != nil {
		return IdentityAssets{}, err
	}
	assets.Management = request.Management
	return assets, nil
}

func WriteManagementIdentity(root, nodeName string, identity managementidentity.NodeCredentials) error {
	if identity == (managementidentity.NodeCredentials{}) {
		return fmt.Errorf("management identity is required")
	}
	credentials := managementidentity.NodeCredentials{
		Authentication:    identity.Authentication,
		CACertificate:     identity.CACertificate,
		ServerCertificate: identity.ServerCertificate,
		ServerPrivateKey:  identity.ServerPrivateKey,
	}
	if err := managementidentity.ValidateNode(credentials, strings.TrimSpace(nodeName), time.Now().UTC()); err != nil {
		return fmt.Errorf("validate management identity: %w", err)
	}
	mode := identity.Authentication
	if mode == "" {
		mode = managementidentity.MutualTLS
	}
	if existing, err := os.ReadFile(filepath.Join(root, ManagementAuthenticationPath)); err == nil {
		if strings.TrimSpace(string(existing)) != string(mode) {
			return fmt.Errorf("existing management authentication differs from install intent")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if mode == managementidentity.TrustedNetwork {
		if _, err := os.Stat(filepath.Join(root, ManagementCACertificatePath)); err == nil {
			return fmt.Errorf("existing mTLS identity must not be replaced implicitly")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	dir := filepath.Join(filepath.Clean(strings.TrimSpace(root)), "var/lib/katl/identity/management")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create management identity directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure management identity directory: %w", err)
	}
	type identityFile struct {
		path string
		data string
		mode os.FileMode
	}
	files := []identityFile{
		{ManagementCACertificatePath, identity.CACertificate, 0o444},
		{ManagementServerCertPath, identity.ServerCertificate, 0o444},
		{ManagementServerPrivateKeyPath, identity.ServerPrivateKey, 0o600},
	}
	if mode == managementidentity.TrustedNetwork {
		files = nil
	}
	// Publish mode last so incomplete mTLS provisioning never opens an unauthenticated listener.
	files = append(files, identityFile{ManagementAuthenticationPath, string(mode) + "\n", 0o600})
	for _, file := range files {
		path := filepath.Join(filepath.Clean(strings.TrimSpace(root)), file.path)
		if existing, err := os.ReadFile(path); err == nil {
			if !bytes.Equal(existing, []byte(file.data)) {
				return fmt.Errorf("existing management identity %s differs from install material", file.path)
			}
			if err := os.Chmod(path, file.mode); err != nil {
				return fmt.Errorf("protect management identity %s: %w", file.path, err)
			}
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read management identity %s: %w", file.path, err)
		}
		if err := os.WriteFile(path, []byte(file.data), file.mode); err != nil {
			return fmt.Errorf("write management identity %s: %w", file.path, err)
		}
		if err := os.Chmod(path, file.mode); err != nil {
			return fmt.Errorf("protect management identity %s: %w", file.path, err)
		}
	}
	return nil
}

func WriteEnrollment(root, nodeName, machineID string, random io.Reader) (Enrollment, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return Enrollment{}, fmt.Errorf("target root is required")
	}
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return Enrollment{}, fmt.Errorf("inventory node name is required")
	}
	machineID, err := ParseMachineID(machineID)
	if err != nil {
		return Enrollment{}, err
	}
	path := filepath.Join(root, EnrollmentPath)
	if data, err := os.ReadFile(path); err == nil {
		enrollment, err := decodeEnrollment(data)
		if err != nil {
			return Enrollment{}, fmt.Errorf("decode enrollment identity: %w", err)
		}
		if enrollment.InventoryNodeName != nodeName || enrollment.MachineID != machineID {
			return Enrollment{}, fmt.Errorf("existing enrollment belongs to inventory node %q and machine %q", enrollment.InventoryNodeName, enrollment.MachineID)
		}
		if err := os.Chmod(path, 0o444); err != nil {
			return Enrollment{}, fmt.Errorf("protect enrollment identity: %w", err)
		}
		return enrollment, nil
	} else if !os.IsNotExist(err) {
		return Enrollment{}, fmt.Errorf("read enrollment identity: %w", err)
	}
	id, err := generateIdentity(random)
	if err != nil {
		return Enrollment{}, fmt.Errorf("generate enrollment identity: %w", err)
	}
	enrollment := Enrollment{APIVersion: "katl.dev/v1alpha1", Kind: "NodeEnrollment", ID: id, InventoryNodeName: nodeName, MachineID: machineID}
	data, err := json.MarshalIndent(enrollment, "", "  ")
	if err != nil {
		return Enrollment{}, fmt.Errorf("encode enrollment identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Enrollment{}, fmt.Errorf("create enrollment identity directory: %w", err)
	}
	if err := writeEnrollmentAtomic(path, append(data, '\n')); err != nil {
		return Enrollment{}, fmt.Errorf("write enrollment identity: %w", err)
	}
	return enrollment, nil
}

func writeEnrollmentAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".enrollment.json.tmp-")
	if err != nil {
		return fmt.Errorf("create temporary enrollment identity: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary enrollment identity: %w", err)
	}
	if err := temporary.Chmod(0o444); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect temporary enrollment identity: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary enrollment identity: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary enrollment identity: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return fmt.Errorf("publish enrollment identity without replacement: %w", err)
	}
	if err := os.Remove(temporaryPath); err != nil {
		return fmt.Errorf("remove temporary enrollment identity: %w", err)
	}
	cleanup = false
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open enrollment identity directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync enrollment identity directory: %w", err)
	}
	return nil
}

func ReadEnrollment(root string) (Enrollment, error) {
	path := filepath.Join(filepath.Clean(strings.TrimSpace(root)), EnrollmentPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return Enrollment{}, fmt.Errorf("read enrollment identity: %w", err)
	}
	enrollment, err := decodeEnrollment(data)
	if err != nil {
		return Enrollment{}, fmt.Errorf("decode enrollment identity: %w", err)
	}
	return enrollment, nil
}

func decodeEnrollment(data []byte) (Enrollment, error) {
	var enrollment Enrollment
	if err := json.Unmarshal(data, &enrollment); err != nil {
		return Enrollment{}, err
	}
	if enrollment.APIVersion != "katl.dev/v1alpha1" || enrollment.Kind != "NodeEnrollment" {
		return Enrollment{}, fmt.Errorf("unsupported enrollment record %s %s", enrollment.APIVersion, enrollment.Kind)
	}
	if _, err := hex.DecodeString(enrollment.ID); err != nil || len(enrollment.ID) != 32 || enrollment.ID != strings.ToLower(enrollment.ID) {
		return Enrollment{}, fmt.Errorf("enrollment id must be 32 lowercase hex characters")
	}
	if strings.TrimSpace(enrollment.InventoryNodeName) == "" {
		return Enrollment{}, fmt.Errorf("inventory node name is required")
	}
	if _, err := ParseMachineID(enrollment.MachineID); err != nil {
		return Enrollment{}, err
	}
	return enrollment, nil
}

func generateIdentity(random io.Reader) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	var data [16]byte
	if _, err := io.ReadFull(random, data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func WriteMachineID(root string, random io.Reader) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("target root is required")
	}
	path := filepath.Join(root, "var/lib/katl/identity/machine-id")
	if data, err := os.ReadFile(path); err == nil {
		machineID, err := ParseMachineID(string(data))
		if err != nil {
			return "", err
		}
		if err := os.Chmod(path, 0o444); err != nil {
			return "", fmt.Errorf("chmod machine-id: %w", err)
		}
		return machineID, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read machine-id: %w", err)
	}
	machineID, err := GenerateMachineID(random)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create machine-id directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(machineID+"\n"), 0o444); err != nil {
		return "", fmt.Errorf("write machine-id: %w", err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		return "", fmt.Errorf("chmod machine-id: %w", err)
	}
	return machineID, nil
}

func GenerateMachineID(random io.Reader) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	var data [16]byte
	if _, err := io.ReadFull(random, data[:]); err != nil {
		return "", fmt.Errorf("generate machine-id: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

func cleanKeys(keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("authorized keys must not be empty")
	}
	cleaned := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("authorized key must not be empty")
		}
		if strings.ContainsAny(key, "\n\r") {
			return nil, fmt.Errorf("authorized key must be a single line")
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		cleaned = append(cleaned, key)
	}
	return cleaned, nil
}

func ParseMachineID(machineID string) (string, error) {
	machineID = strings.TrimSpace(machineID)
	if len(machineID) != 32 {
		return "", fmt.Errorf("machine id must be 32 lowercase hex characters")
	}
	if machineID != strings.ToLower(machineID) {
		return "", fmt.Errorf("machine id must be lowercase hex")
	}
	if _, err := hex.DecodeString(machineID); err != nil {
		return "", fmt.Errorf("machine id is invalid: %w", err)
	}
	return machineID, nil
}

func ManagementAuthentication(root string) (managementidentity.Authentication, error) {
	data, err := os.ReadFile(filepath.Join(root, ManagementAuthenticationPath))
	if os.IsNotExist(err) {
		return managementidentity.MutualTLS, nil
	}
	if err != nil {
		return "", err
	}
	mode := managementidentity.Authentication(strings.TrimSpace(string(data)))
	if mode == "" {
		return "", fmt.Errorf("installed management authentication is empty")
	}
	return mode, mode.Validate()
}
