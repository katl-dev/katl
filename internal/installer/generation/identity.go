package generation

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type IdentityRequest struct {
	AuthorizedKeys []string
	Random         io.Reader
}

type IdentityAssets struct {
	MachineID      string
	SSHDConfig     string
	AuthorizedKeys string
	Sysusers       string
	HostKeyService string
	SSHDUnitDropIn string
}

func RenderSSH(keys []string) (IdentityAssets, error) {
	cleaned, err := cleanKeys(keys)
	if err != nil {
		return IdentityAssets{}, err
	}
	return IdentityAssets{
		AuthorizedKeys: strings.Join(append(cleaned, ""), "\n"),
		SSHDConfig: strings.Join([]string{
			"PasswordAuthentication no",
			"KbdInteractiveAuthentication no",
			"PubkeyAuthentication yes",
			"PermitRootLogin no",
			"AuthorizedKeysFile /etc/ssh/authorized_keys/%u",
			"AllowUsers katl",
			"HostKey /var/lib/katl/ssh/host-keys/ssh_host_ed25519_key",
			"",
		}, "\n"),
		Sysusers: strings.Join([]string{
			"# Katl-owned host accounts",
			"u katl - \"Katl operator\" /home/katl /usr/bin/bash",
			"",
		}, "\n"),
		HostKeyService: strings.Join([]string{
			"[Unit]",
			"Description=Generate Katl SSH host keys",
			"ConditionPathExists=!/var/lib/katl/ssh/host-keys/ssh_host_ed25519_key",
			"RequiresMountsFor=/var/lib/katl/ssh/host-keys",
			"After=var.mount",
			"Before=sshd.service",
			"",
			"[Service]",
			"Type=oneshot",
			"ExecStart=/usr/bin/ssh-keygen -q -t ed25519 -N \"\" -f /var/lib/katl/ssh/host-keys/ssh_host_ed25519_key",
			"",
		}, "\n"),
		SSHDUnitDropIn: strings.Join([]string{
			"[Unit]",
			"Wants=katl-ssh-host-keys.service",
			"After=katl-ssh-host-keys.service",
			"",
		}, "\n"),
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
	files := []struct {
		path    string
		content string
		mode    os.FileMode
	}{
		{path: "etc/ssh/authorized_keys/katl", content: assets.AuthorizedKeys, mode: 0o600},
		{path: "etc/ssh/sshd_config.d/10-katl.conf", content: assets.SSHDConfig, mode: 0o600},
		{path: "etc/sysusers.d/10-katl-users.conf", content: assets.Sysusers, mode: 0o644},
		{path: "etc/systemd/system/katl-ssh-host-keys.service", content: assets.HostKeyService, mode: 0o644},
		{path: "etc/systemd/system/sshd.service.d/10-katl-host-keys.conf", content: assets.SSHDUnitDropIn, mode: 0o644},
	}
	for _, file := range files {
		if err := writeFile(root, file.path, file.content, file.mode); err != nil {
			return IdentityAssets{}, err
		}
		if err := os.Chmod(filepath.Join(root, filepath.FromSlash(file.path)), file.mode); err != nil {
			return IdentityAssets{}, fmt.Errorf("chmod %s: %w", file.path, err)
		}
	}
	return assets, nil
}

func WriteMachineID(root string, random io.Reader) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("target root is required")
	}
	path := filepath.Join(root, "var/lib/katl/identity/machine-id")
	if data, err := os.ReadFile(path); err == nil {
		machineID, err := cleanMachineID(string(data))
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

type InstallIdentityRequest struct {
	TargetRoot string
	BootRoot   string
	Identity   IdentityRequest
	Loader     LoaderRequest
}

type InstallIdentity struct {
	Identity  IdentityAssets
	EntryPath string
}

func WriteInstallIdentity(request InstallIdentityRequest) (InstallIdentity, error) {
	if strings.TrimSpace(request.BootRoot) == "" {
		return InstallIdentity{}, fmt.Errorf("boot root is required")
	}
	identity, err := WriteIdentity(request.TargetRoot, request.Identity)
	if err != nil {
		return InstallIdentity{}, err
	}
	loader := request.Loader
	loader.MachineID = identity.MachineID
	entryPath, err := WriteEntry(request.BootRoot, loader)
	if err != nil {
		return InstallIdentity{}, err
	}
	return InstallIdentity{Identity: identity, EntryPath: entryPath}, nil
}
