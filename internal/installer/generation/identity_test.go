package generation

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKatlExampleRuntimeKeyReplaceMe katl@example"

func TestMachineID(t *testing.T) {
	id, err := GenerateMachineID(bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}))
	if err != nil {
		t.Fatalf("GenerateMachineID() error = %v", err)
	}
	if id != "000102030405060708090a0b0c0d0e0f" {
		t.Fatalf("machine id = %q", id)
	}
}

func TestWriteMachineID(t *testing.T) {
	root := t.TempDir()
	id, err := WriteMachineID(root, bytes.NewReader([]byte("0123456789abcdef")))
	if err != nil {
		t.Fatalf("WriteMachineID() error = %v", err)
	}
	if id != "30313233343536373839616263646566" {
		t.Fatalf("machine id = %q", id)
	}
	path := filepath.Join(root, "var/lib/katl/identity/machine-id")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read machine-id: %v", err)
	}
	if string(data) != id+"\n" {
		t.Fatalf("machine-id file = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat machine-id: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o444 {
		t.Fatalf("machine-id mode = %04o, want 0444", got)
	}

	reused, err := WriteMachineID(root, bytes.NewReader([]byte("xxxxxxxxxxxxxxxx")))
	if err != nil {
		t.Fatalf("WriteMachineID() reuse error = %v", err)
	}
	if reused != id {
		t.Fatalf("reused machine id = %q, want %q", reused, id)
	}
}

func TestRenderSSH(t *testing.T) {
	assets, err := RenderSSH([]string{sshKey, sshKey})
	if err != nil {
		t.Fatalf("RenderSSH() error = %v", err)
	}
	if strings.Count(assets.AuthorizedKeys, sshKey) != 1 {
		t.Fatalf("authorized keys = %q", assets.AuthorizedKeys)
	}
	for _, want := range []string{
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"PermitRootLogin no",
		"AllowUsers katl",
		"AuthorizedKeysFile /etc/ssh/authorized_keys/%u",
		"HostKey /var/lib/katl/ssh/host-keys/ssh_host_ed25519_key",
	} {
		if !strings.Contains(assets.SSHDConfig, want) {
			t.Fatalf("sshd config missing %q:\n%s", want, assets.SSHDConfig)
		}
	}
	if !strings.Contains(assets.Sysusers, "u katl") || strings.Contains(assets.Sysusers, "root") {
		t.Fatalf("sysusers = %q", assets.Sysusers)
	}
	if !strings.Contains(assets.HostKeyService, "ssh-keygen") || !strings.Contains(assets.SSHDUnitDropIn, "katl-ssh-host-keys.service") {
		t.Fatalf("host key service/drop-in = %q / %q", assets.HostKeyService, assets.SSHDUnitDropIn)
	}
}

func TestWriteIdentity(t *testing.T) {
	root := t.TempDir()
	assets, err := WriteIdentity(root, IdentityRequest{
		AuthorizedKeys: []string{sshKey},
		Random:         bytes.NewReader([]byte("0123456789abcdef")),
	})
	if err != nil {
		t.Fatalf("WriteIdentity() error = %v", err)
	}
	assertFile(t, filepath.Join(root, "var/lib/katl/identity/machine-id"), assets.MachineID+"\n")
	assertFile(t, filepath.Join(root, "etc/ssh/authorized_keys/katl"), assets.AuthorizedKeys)
	assertFile(t, filepath.Join(root, "etc/ssh/sshd_config.d/10-katl.conf"), assets.SSHDConfig)
	assertFile(t, filepath.Join(root, "etc/sysusers.d/10-katl-users.conf"), assets.Sysusers)
	assertFile(t, filepath.Join(root, "etc/systemd/system/katl-ssh-host-keys.service"), assets.HostKeyService)
	assertFile(t, filepath.Join(root, "etc/systemd/system/sshd.service.d/10-katl-host-keys.conf"), assets.SSHDUnitDropIn)
	assertMode(t, filepath.Join(root, "etc/ssh/authorized_keys/katl"), 0o600)
	assertMode(t, filepath.Join(root, "etc/ssh/sshd_config.d/10-katl.conf"), 0o600)
}

func TestRenderSSHRejectsKey(t *testing.T) {
	_, err := RenderSSH([]string{"ssh-ed25519 AAA\nssh-rsa BBB"})
	if err == nil || !strings.Contains(err.Error(), "single line") {
		t.Fatalf("RenderSSH() error = %v, want single-line key failure", err)
	}
}

func assertMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Fatalf("%s mode = %04o, want %04o", path, got, mode)
	}
}

func TestWriteMachineIDProtectsExisting(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "var/lib/katl/identity/machine-id")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir machine-id dir: %v", err)
	}
	if err := os.WriteFile(path, []byte("0123456789abcdef0123456789abcdef\n"), 0o666); err != nil {
		t.Fatalf("write machine-id: %v", err)
	}

	id, err := WriteMachineID(root, bytes.NewReader([]byte("xxxxxxxxxxxxxxxx")))
	if err != nil {
		t.Fatalf("WriteMachineID() error = %v", err)
	}
	if id != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("machine id = %q", id)
	}
	assertMode(t, path, 0o444)
}

func TestWriteInstallIdentity(t *testing.T) {
	targetRoot := t.TempDir()
	bootRoot := t.TempDir()
	record := abRecord(t, "2026.06.01-005", "root-a", "11111111-2222-3333-4444-555555555555", "0.1.0", "v1.34.8", time.Time{})

	result, err := WriteInstallIdentity(InstallIdentityRequest{
		TargetRoot: targetRoot,
		BootRoot:   bootRoot,
		Identity: IdentityRequest{
			AuthorizedKeys: []string{sshKey},
			Random:         bytes.NewReader([]byte("0123456789abcdef")),
		},
		Loader: LoaderRequest{Record: record},
	})
	if err != nil {
		t.Fatalf("WriteInstallIdentity() error = %v", err)
	}
	assertFile(t, filepath.Join(targetRoot, "var/lib/katl/identity/machine-id"), result.Identity.MachineID+"\n")
	data, err := os.ReadFile(result.EntryPath)
	if err != nil {
		t.Fatalf("read loader entry: %v", err)
	}
	if !strings.Contains(string(data), "systemd.machine_id="+result.Identity.MachineID) {
		t.Fatalf("loader entry missing generated machine id:\n%s", data)
	}
}
