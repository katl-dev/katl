package vmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
)

func TestInstalledRuntimeManagementRotationSmoke(t *testing.T) {
	runManagementIdentityJourney(t, "installed-runtime-management-rotation", "rotate")
}

func TestManagementIdentityRecovery(t *testing.T) {
	runManagementIdentityJourney(t, "management-identity-recovery", "recover")
}

func stashRecoverySecret(path string) error {
	// The run directory and the test temp directory can be on different
	// filesystems. Keep the rename on the filesystem that holds the secret.
	dir, err := os.MkdirTemp(filepath.Dir(path), "lost-management-")
	if err != nil {
		return err
	}
	if err := os.Rename(path, filepath.Join(dir, filepath.Base(path))); err != nil {
		_ = os.Remove(dir)
		return err
	}
	return nil
}

func TestRecoveryFixtureStashesSecretsOnSourceFilesystem(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "management-secrets.yaml")
	if err := os.WriteFile(path, []byte("fixture issuer"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := stashRecoverySecret(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("issuer remains at its lookup path: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("found %v, %v; want a backup directory beside the source", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name(), filepath.Base(path)))
	if err != nil || string(data) != "fixture issuer" {
		t.Fatalf("could not read the relocated issuer: %v", err)
	}
}

func runManagementIdentityJourney(t *testing.T, scenarioName, command string) {
	t.Helper()
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run the management identity journey")
	}
	sshDir := t.TempDir()
	keyPath := filepath.Join(sshDir, "id_ed25519")
	if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("generate SSH fixture key: %v: %s", err, output)
	}
	publicKey, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	sshAuthorizedKey := strings.TrimSpace(string(publicKey))
	runner := NewRunner(options)
	runtime := InstalledRuntimeConfig{}
	var plannedAddress string
	var worldScenario *WorldScenario
	if worldRun, ok := installedRuntimeWorldRunFor(t, scenarioName, NodeSpec{Name: "cp-1", Role: ControlPlane}, sshAuthorizedKey); ok {
		runner, runtime = worldRun.Runner, worldRun.Config
		plannedAddress = worldRun.Node.Address
		worldScenario = worldRun.Scenario
	} else {
		_ = RequireWorld(t)
	}
	scenario := Scenario{Name: scenarioName}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatal(err)
	}
	result = requirePlannedVMHost(t, runner, scenario, result, HostRequirements{Libvirt: true, OVMF: true, KVM: runner.options().KVM})

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	katlctl := configApplyKatlctl(BuildKatlctl(t, ctx, repoRoot(t)), result)
	vm := runtime.VM
	vm.KVM = runner.options().KVM
	vm.RAMMiB, vm.CPUs = 2048, 2
	vm.Timeout = 10 * time.Minute
	vm.VSock.Enabled = true
	vm.Agent.RequireHealth = true
	vm.Agent.Timeout = 30 * time.Second
	vm.PreserveNVRAM = true
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
		Name: "cp-1",
		Runtime: InstalledRuntimeConfig{
			Disk: runtime.Disk, DiskFormat: runtime.DiskFormat, ESPArtifacts: runtime.ESPArtifacts,
			FixtureManifest: runtime.FixtureManifest, NodeMetadata: runtime.NodeMetadata, VM: vm,
		},
	}, VMRunner{})
	if err != nil {
		t.Fatalf("start installed node: %v", err)
	}
	defer func() {
		if t.Failed() {
			_ = node.StopFailure("management identity journey failed")
		} else {
			_ = node.Stop()
		}
	}()
	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()
	guest := NewGuestControl(node.Result, client)
	assertInstalledSSHReady(t, ctx, guest)
	endpoint := katlcEndpoint(t, node, plannedAddress)
	enrollConfigApplyNode(t, ctx, result, katlctl, endpoint, sshAuthorizedKey)
	configPath := filepath.Join(result.RunDir, "katlctl", "enrollment", "cluster.yaml")
	old, err := VMTestManagementIdentity(VMTestManagementClusterName)
	if err != nil {
		t.Fatal(err)
	}
	originalAuthority, err := managementidentity.CAFingerprint(old.CertificateAuthority.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	assertManagementIdentityStatus(t, ctx, result, katlctl, "identity-before", originalAuthority)

	hostKey := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "identity-host-key", "dd", "if=/var/lib/katl/ssh/host-keys/ssh_host_ed25519_key.pub", "status=none"))
	if !strings.HasPrefix(hostKey, "ssh-ed25519 ") {
		t.Fatalf("unexpected installed SSH host key: %q", hostKey)
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(sshDir, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(fmt.Sprintf("%s %s\n", host, hostKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	unknownHosts := filepath.Join(sshDir, "unknown_hosts")
	if err := os.WriteFile(unknownHosts, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(filepath.Dir(configPath), "management-next.yaml")
	if command == "recover" {
		oldPath := filepath.Join(filepath.Dir(configPath), "management-secrets.yaml")
		runKatlctl(t, ctx, result, katlctl, "identity-export", "management", "identity", "export", "--config", configPath)
		// Move both issuer copies out of the configuration and legacy lookup
		// paths. Preserve the old saved client to check its access afterwards.
		if err := stashRecoverySecret(oldPath); err != nil {
			t.Fatal(err)
		}
		legacyPath := filepath.Join(filepath.Dir(katlctl.config), "management", VMTestManagementClusterName+".katlkey")
		if err := stashRecoverySecret(legacyPath); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	args := []string{"management", "identity", command, "--config", configPath, "--output", outputPath, "--ssh-key", keyPath, "--ssh-known-hosts"}
	if _, err := runKatlctlOutcome(t, ctx, result, katlctl, "identity-unknown-host", append(args, unknownHosts)...); err == nil || !strings.Contains(err.Error(), "Host key verification failed") {
		t.Fatalf("unknown SSH host key was not rejected: %v", err)
	}
	assertManagementIdentityStatus(t, ctx, result, katlctl, "identity-after-preflight", originalAuthority)
	runKatlctl(t, ctx, result, katlctl, "identity-switch", append(args, knownHosts)...)
	if _, err := runKatlctlOutcome(t, ctx, result, katlctl, "identity-old-client", "node", "status", "cp-1"); err == nil {
		t.Fatal("old saved management credentials still have access")
	}
	replacement, _, err := managementidentity.Read(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := managementidentity.CAFingerprint(replacement.CertificateAuthority.Certificate)
	if err != nil || authority == originalAuthority {
		t.Fatalf("replacement authority = %q, %v", authority, err)
	}
	runKatlctl(t, ctx, result, katlctl, "identity-context-refresh", "context", "save", "--config", configPath)
	assertManagementIdentityStatus(t, ctx, result, katlctl, "identity-new-client", authority)
	repeated := runKatlctl(t, ctx, result, katlctl, "identity-repeat", append(args, knownHosts)...)
	want := "already uses"
	if command == "recover" {
		want = "nothing to recover"
	}
	if !strings.Contains(string(repeated), want) {
		t.Fatalf("repeat %s = %s, want %q", command, repeated, want)
	}
	previousBootID := guestBootID(t, ctx, client)
	runKatlctl(t, ctx, result, katlctl, "identity-reboot", "node", "reboot", "cp-1")
	_ = client.Close()
	guest, client = reconnectGuestAfterBoot(t, ctx, &node, previousBootID)
	assertManagementIdentityStatus(t, ctx, result, katlctl, "identity-after-reboot", authority)
	guestCommand(t, ctx, guest, "identity-agent-active", "systemctl", "is-active", "--quiet", "katlc-agent.service")

	node.Result.finish(StatusPassed, "", runner.time())
	if err := runner.Write(scenario, node.Result); err != nil {
		t.Fatal(err)
	}
	if worldScenario != nil {
		if err := worldScenario.WriteResult(WorldStatusPassed, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func assertManagementIdentityStatus(t *testing.T, ctx context.Context, result Result, katlctl katlctlRunner, step, authority string) {
	t.Helper()
	output := runKatlctl(t, ctx, result, katlctl, step, "node", "status", "cp-1", "--output", "json")
	var status struct {
		Management struct {
			Authentication    string
			Authority         string
			ServedClientTrust *struct {
				Authorities    []string
				TrialID        string
				TrialAuthority string
			}
		}
	}
	if err := json.Unmarshal(output, &status); err != nil {
		t.Fatalf("decode %s status: %v", step, err)
	}
	management := status.Management
	trust := management.ServedClientTrust
	if management.Authentication != "mtls" || management.Authority != authority || trust == nil ||
		!slices.Equal(trust.Authorities, []string{authority}) || trust.TrialID != "" || trust.TrialAuthority != "" {
		t.Fatalf("%s did not report committed mTLS trusting only %s: %s", step, authority, output)
	}
}
