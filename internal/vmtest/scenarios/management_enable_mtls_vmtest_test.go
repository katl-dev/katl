package scenarios

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/katl-dev/katl/internal/vmtest"
)

// An operator moves an installed trusted-network node to mTLS management with
// one command. A run interrupted after the node began its trial, and a run
// whose mTLS check cannot reach the node, both leave the node to return to
// trusted-network by its deadline; a rerun with the same secrets finishes, a
// second rerun changes nothing, and the node keeps mTLS across a reboot.
func TestManagementEnableMTLS(t *testing.T) {
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run the enable-mtls journey")
	}
	world := vmtest.RequireWorld(t)
	worldScenario := world.NewScenario(t, "management-enable-mtls")
	options.StateRoot = filepath.Join(worldScenario.Dir, "vm-runs")
	options.Keep = vmtest.KeepFailed
	fail := func(err error) {
		t.Helper()
		_ = worldScenario.WriteResult(vmtest.WorldStatusFailed, err.Error())
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	repo := katlRepoRoot(t)
	katlctl := buildKatlctlCommand(t, ctx, repo, filepath.Join(worldScenario.Dir, "katlctl.yaml"))
	privateKey, _, err := ensureWorldSSHKey(world)
	if err != nil {
		fail(err)
	}
	installerUKI, err := worldArtifactPath(world, "installer-uki")
	if err != nil {
		fail(err)
	}
	image, err := worldArtifactPath(world, "katlos-install-image")
	if err != nil {
		fail(err)
	}
	node, err := worldScenario.AddNode(vmtest.NodeSpec{Name: "cp-1", Role: vmtest.ControlPlane})
	if err != nil {
		fail(err)
	}
	disk := filepath.Join(worldScenario.Dir, "root.raw")
	if err := writeInstallerESPDisk(ctx, worldScenario.Dir, disk, installerUKI); err != nil {
		fail(err)
	}
	imageURL, _, stopImageServer, err := serveInstallImage(worldScenario.World.Network.Gateway, image)
	if err != nil {
		fail(err)
	}
	defer stopImageServer()

	runner := vmtest.NewRunner(options)
	scenario := vmtest.Scenario{Name: "management-enable-mtls"}
	result, err := runner.Plan(scenario)
	if err != nil {
		fail(err)
	}
	vm := vmtest.VMConfig{
		Boot:          vmtest.VMBoot{Image: disk, ImageFormat: vmtest.DiskRaw, ImageWithoutSerial: true},
		Network:       node.VMNetwork(),
		PreserveNVRAM: true,
		KVM:           options.KVM,
		RAMMiB:        2048,
		CPUs:          2,
		Timeout:       30 * time.Minute,
	}
	journey := enableMTLSJourney{installerESPJourney: installerESPJourney{
		katlctl:    katlctl,
		privateKey: privateKey,
		publicKey:  privateKey + ".pub",
		dir:        worldScenario.Dir,
		imageURL:   imageURL,
		imageJSON:  image + ".json",
	}}
	// The installer journey installs cp-1 with trusted-network management.
	result = vmtest.VMRunner{}.RunWithVM(ctx, result, vm, func(handle *vmtest.VMHandle) error {
		return journey.installAndPowerOff(ctx, handle)
	})
	if result.Status != vmtest.StatusPassed {
		_ = runner.Write(scenario, result)
		fail(fmt.Errorf("install journey: %s (%s)", result.FailureSummary, result.RunDir))
	}

	vm.OVMFVars = filepath.Join(result.VMDir, "OVMF_VARS.fd")
	vm.Timeout = 25 * time.Minute
	result = vmtest.VMRunner{}.RunWithVM(ctx, result, vm, func(*vmtest.VMHandle) error {
		return journey.migrate(ctx)
	})
	if err := runner.Write(scenario, result); err != nil {
		fail(err)
	}
	if result.Status != vmtest.StatusPassed {
		fail(fmt.Errorf("enable-mtls journey: %s (%s)", result.FailureSummary, result.RunDir))
	}
	if err := worldScenario.WriteResult(vmtest.WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}

type enableMTLSJourney struct {
	installerESPJourney
}

func (j *enableMTLSJourney) migrate(ctx context.Context) error {
	if _, err := j.waitInstalledBoot(ctx, ""); err != nil {
		return err
	}
	config := filepath.Join(j.dir, "cluster.yaml")
	if err := j.requireGeneration0(ctx, config); err != nil {
		return err
	}
	if err := requireConfigMode(config, "trusted-network"); err != nil {
		return err
	}
	trustedConfig := filepath.Join(j.dir, "trusted", "cluster.yaml")
	if err := os.MkdirAll(filepath.Dir(trustedConfig), 0o700); err != nil {
		return err
	}
	if err := copyFile(config, trustedConfig); err != nil {
		return err
	}
	agentPID, err := j.ssh(ctx, "systemctl show --property=MainPID --value katlc-agent.service")
	if err != nil {
		return err
	}

	// The proxied config reaches the node through a path that carries
	// plaintext management but drops TLS, as a firewall that admits only the
	// old protocol would. It shares cluster.yaml's directory, so both use the
	// same default secrets file.
	proxy, err := startPlaintextOnlyProxy(net.JoinHostPort(j.address, "9443"))
	if err != nil {
		return err
	}
	defer proxy.Close()
	proxiedConfig := filepath.Join(j.dir, "cluster-proxied.yaml")
	if err := writeProxiedConfig(config, proxiedConfig, proxy.Addr().String()); err != nil {
		return err
	}

	stderr, err := j.interruptAfterTrialBegins(ctx, "management", "enable-mtls", "--config", proxiedConfig, "--trial-window", "45s")
	if err == nil {
		return fmt.Errorf("interrupted enable-mtls succeeded:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(j.dir, "management-secrets.yaml")); err != nil {
		return fmt.Errorf("the interrupted run left no secrets file: %w", err)
	}
	for _, want := range []string{"interrupted before the new authority was confirmed", "returns to trusted-network by itself"} {
		if !strings.Contains(stderr, want) {
			return fmt.Errorf("interrupted run output lacks %q:\n%s", want, stderr)
		}
	}
	if err := requireConfigMode(config, "trusted-network"); err != nil {
		return err
	}
	if err := j.waitReverted(ctx, config); err != nil {
		return fmt.Errorf("after the interrupted run: %w", err)
	}

	_, err = j.cli(ctx, "management", "enable-mtls", "--config", proxiedConfig, "--trial-window", "45s", "--timeout", "20s")
	if err == nil || !strings.Contains(err.Error(), "did not answer over mTLS with the new authority") || !strings.Contains(err.Error(), "returns to trusted-network by itself") {
		return fmt.Errorf("unreachable mTLS check: %v", err)
	}
	if err := j.waitReverted(ctx, config); err != nil {
		return fmt.Errorf("after the unreachable mTLS check: %w", err)
	}

	command, err := j.katlctl.command(ctx, "management", "enable-mtls", "--config", config)
	if err != nil {
		return err
	}
	var progress strings.Builder
	command.Stderr = &progress
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("enable-mtls: %w\n%s%s", err, output, progress.String())
	}
	for _, want := range []string{"Using management secrets", "an earlier trial reverted", "cp-1: mTLS confirmed (1/1)"} {
		if !strings.Contains(progress.String(), want) {
			return fmt.Errorf("enable-mtls progress lacks %q:\n%s", want, progress.String())
		}
	}
	if strings.Contains(string(output), "Created management secrets") || !strings.Contains(string(output), "now uses mTLS management") {
		return fmt.Errorf("enable-mtls did not finish with the existing secrets:\n%s", output)
	}
	if err := requireConfigMode(config, "mtls"); err != nil {
		return err
	}
	if err := j.requireManagement(ctx, config, "mtls"); err != nil {
		return err
	}
	if _, err := j.cli(ctx, "node", "status", "cp-1", "--config", trustedConfig); err == nil {
		return errors.New("the node still answers its trusted-network configuration")
	}
	if pid, err := j.ssh(ctx, "systemctl show --property=MainPID --value katlc-agent.service"); err != nil || pid != agentPID {
		return fmt.Errorf("katlc-agent main PID = %q (%v), want %s: the migration restarted the agent", pid, err, agentPID)
	}

	output, err = j.cli(ctx, "management", "enable-mtls", "--config", config)
	if err != nil || !strings.Contains(string(output), "already uses mTLS") {
		return fmt.Errorf("rerun = %v:\n%s", err, output)
	}
	if _, err := j.cli(ctx, "context", "save", "--config", config); err != nil {
		return err
	}
	if _, err := j.cli(ctx, "node", "status", "cp-1"); err != nil {
		return fmt.Errorf("saved context after migration: %w", err)
	}

	boot, err := j.ssh(ctx, "cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	if _, err := j.cli(ctx, "node", "reboot", "cp-1", "--config", config); err != nil {
		return err
	}
	if _, err := j.waitInstalledBoot(ctx, boot); err != nil {
		return fmt.Errorf("reboot after migration: %w", err)
	}
	if err := j.requireGeneration0(ctx, config); err != nil {
		return fmt.Errorf("after reboot: %w", err)
	}
	if err := j.requireManagement(ctx, config, "mtls"); err != nil {
		return fmt.Errorf("after reboot: %w", err)
	}
	if _, err := j.cli(ctx, "node", "status", "cp-1", "--config", trustedConfig); err == nil {
		return errors.New("after reboot the node answers its trusted-network configuration")
	}
	return nil
}

// interruptAfterTrialBegins sends SIGINT, as Ctrl-C does, once katlctl reports
// that the node serves the new authority on trial.
func (j *enableMTLSJourney) interruptAfterTrialBegins(ctx context.Context, args ...string) (string, error) {
	command, err := j.katlctl.command(ctx, args...)
	if err != nil {
		return "", err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return "", err
	}
	command.Stdout = io.Discard
	if err := command.Start(); err != nil {
		return "", err
	}
	var output strings.Builder
	scanner := bufio.NewScanner(stderr)
	interrupted := false
	for scanner.Scan() {
		output.WriteString(scanner.Text() + "\n")
		if !interrupted && strings.Contains(scanner.Text(), "serving the new authority on trial") {
			interrupted = true
			if err := command.Process.Signal(syscall.SIGINT); err != nil {
				return output.String(), err
			}
		}
	}
	err = command.Wait()
	if !interrupted {
		return output.String(), fmt.Errorf("katlctl never reported a trial: %v", err)
	}
	return output.String(), err
}

// waitReverted waits for the node to answer its trusted-network config again,
// reporting the reverted trial.
func (j *enableMTLSJourney) waitReverted(ctx context.Context, config string) error {
	deadline, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var last error
	for {
		report, err := j.management(deadline, config)
		if err == nil && report.Authentication == "trusted-network" && report.Transition != nil && report.Transition.State == "reverted" {
			return nil
		}
		last = fmt.Errorf("management %+v: %v", report, err)
		select {
		case <-deadline.Done():
			return fmt.Errorf("the node did not return to trusted-network: %v", last)
		case <-time.After(5 * time.Second):
		}
	}
}

type managementReport struct {
	Authentication string `json:"authentication"`
	Authority      string `json:"authority"`
	Transition     *struct {
		State        string `json:"state"`
		RevertReason string `json:"revertReason"`
	} `json:"transition"`
}

func (j *enableMTLSJourney) management(ctx context.Context, config string) (managementReport, error) {
	output, err := j.cli(ctx, "node", "status", "cp-1", "--config", config, "--output", "json")
	if err != nil {
		return managementReport{}, err
	}
	var status struct {
		Management *managementReport `json:"management"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return managementReport{}, fmt.Errorf("decode node status: %w: %s", err, output)
	}
	if status.Management == nil {
		return managementReport{}, fmt.Errorf("node status reports no management mode: %s", output)
	}
	return *status.Management, nil
}

func (j *enableMTLSJourney) requireManagement(ctx context.Context, config, mode string) error {
	report, err := j.management(ctx, config)
	if err != nil {
		return err
	}
	if report.Authentication != mode || (mode == "mtls" && report.Authority == "") {
		return fmt.Errorf("node management = %+v, want %s", report, mode)
	}
	return nil
}

func requireConfigMode(path, mode string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var config struct {
		Spec struct {
			ManagementAuthentication string `yaml:"managementAuthentication"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return err
	}
	if config.Spec.ManagementAuthentication != mode {
		return fmt.Errorf("%s says managementAuthentication %q, want %q", path, config.Spec.ManagementAuthentication, mode)
	}
	return nil
}

// writeProxiedConfig copies the single-node config with its management
// address replaced by the proxy's.
func writeProxiedConfig(from, to, address string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	replaced := 0
	var walk func(*yaml.Node, bool)
	walk = func(node *yaml.Node, inManagement bool) {
		for i := 0; node.Kind == yaml.MappingNode && i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if inManagement && key.Value == "address" {
				value.Value = address
				replaced++
			}
			walk(value, key.Value == "management")
		}
		for _, child := range node.Content {
			if node.Kind != yaml.MappingNode {
				walk(child, false)
			}
		}
	}
	walk(&document, false)
	if replaced != 1 {
		return fmt.Errorf("%s has %d management addresses, want 1", from, replaced)
	}
	out, err := yaml.Marshal(&document)
	if err != nil {
		return err
	}
	return os.WriteFile(to, out, 0o600)
}

// startPlaintextOnlyProxy forwards TCP to target but closes any connection
// that opens with a TLS record.
func startPlaintextOnlyProxy(target string) (net.Listener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				first := make([]byte, 1)
				if _, err := io.ReadFull(client, first); err != nil || first[0] == 0x16 {
					return
				}
				upstream, err := net.DialTimeout("tcp", target, 5*time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				if _, err := upstream.Write(first); err != nil {
					return
				}
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
				go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return listener, nil
}
