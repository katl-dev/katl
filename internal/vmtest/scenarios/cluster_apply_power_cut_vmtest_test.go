package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/vmtest"
	"gopkg.in/yaml.v3"
)

const (
	powerCutSysctl     = "vm.vfs_cache_pressure"
	powerCutSysctlFile = "/etc/sysctl.d/96-katl-vmtest-power-cut.conf"
	powerCutRecovery   = 6 * time.Minute
)

// powerCutDelays are the times between an apply returning and the power cut.
// The first is immediate: nothing may run on the node in between.
var powerCutDelays = []time.Duration{0, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second}

var cmdlineGeneration = regexp.MustCompile(`(?:^|\s)katl\.generation=(\S+)`)

// runPowerCutJourney cuts power to cp-1 at several points after a live apply
// that changes a sysctl, and during a reboot. Each boot must come back on the
// generation that boot selection names, healthy and reachable.
func runPowerCutJourney(ctx context.Context, katlctl katlctlRunner, dir string, source configbundle.SourceConfig, planning configbundle.PlanningInputs, validConfig string, node *vmtest.RunningInstalledRuntimeNode, address string) error {
	// Sources stay beside the files they name; command evidence gets its own directory.
	sourceDir := dir
	dir = filepath.Join(dir, "power-cut")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	status, err := readAgentNodeStatus(ctx, node.Name, address)
	if err != nil {
		return err
	}
	generation := status.GetCurrentGenerationId()
	if value, err := nodeSysctl(ctx, *node); err != nil || value == "" {
		return fmt.Errorf("read %s before power cut journey: %q: %w", powerCutSysctl, value, err)
	}

	for i, delay := range powerCutDelays {
		label := fmt.Sprintf("apply-delay-%ds", int(delay/time.Second))
		value := strconv.Itoa(110 + i)
		configPath, err := writePowerCutConfig(sourceDir, "power-cut-"+label, source, planning, node.Name, value)
		if err != nil {
			return err
		}
		before, err := nodeBootID(ctx, *node)
		if err != nil {
			return err
		}

		stdout, stderr, err := runProofKatlctl(ctx, katlctl, dir, label, "cluster", "apply", "--config", configPath, "--node", node.Name, "-o", "json")
		if err != nil {
			return fmt.Errorf("%s: apply: %w: %s", label, err, stderr)
		}
		// Nothing may touch the guest between the apply returning and the cut.
		if delay > 0 {
			time.Sleep(delay)
		}
		cycled, err := node.PowerCycle(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		*node = cycled

		var report struct{ Result string }
		if err := json.Unmarshal(stdout, &report); err != nil || report.Result != "succeeded" {
			return fmt.Errorf("%s: unexpected apply result: %s", label, stdout)
		}
		next, err := assertPowerCutRecovery(ctx, katlctl, dir, label, *node, before, generation, value)
		if err != nil {
			return err
		}
		if next == generation {
			return fmt.Errorf("%s: apply did not create a generation; still %s", label, next)
		}
		generation = next
	}

	before, err := nodeBootID(ctx, *node)
	if err != nil {
		return err
	}
	if _, stderr, err := runProofKatlctl(ctx, katlctl, dir, "reboot-cut", "node", "reboot", node.Name, "--config", validConfig, "--no-wait"); err != nil {
		return fmt.Errorf("start reboot: %w: %s", err, stderr)
	}
	// Let the guest begin stopping so that the cut lands mid-reboot.
	time.Sleep(2 * time.Second)
	cycled, err := node.PowerCycle(ctx)
	if err != nil {
		return fmt.Errorf("reboot-cut: %w", err)
	}
	*node = cycled
	last := strconv.Itoa(110 + len(powerCutDelays) - 1)
	next, err := assertPowerCutRecovery(ctx, katlctl, dir, "reboot-cut", *node, before, generation, last)
	if err != nil {
		return err
	}
	if next != generation {
		return fmt.Errorf("reboot-cut: generation changed from %s to %s without an apply", generation, next)
	}

	// Later journeys expect the shared configuration on every node.
	if _, stderr, err := runProofKatlctl(ctx, katlctl, dir, "restore", "cluster", "apply", "--config", validConfig, "--node", node.Name, "-o", "json"); err != nil {
		return fmt.Errorf("restore %s configuration: %w: %s", node.Name, err, stderr)
	}
	return nil
}

func writePowerCutConfig(dir, label string, source configbundle.SourceConfig, planning configbundle.PlanningInputs, nodeName, value string) (string, error) {
	next := source
	next.Spec.Nodes = append([]configbundle.SourceNode(nil), source.Spec.Nodes...)
	content := powerCutSysctl + " = " + value + "\n"
	for i := range next.Spec.Nodes {
		if next.Spec.Nodes[i].Name != nodeName {
			continue
		}
		sets, _ := next.Spec.Nodes[i].HostConfiguration.FileSets.Get()
		merged := map[string]configbundle.SourceHostConfigurationFileSet{}
		for name, set := range sets {
			merged[name] = set
		}
		merged["vmtest-power-cut"] = configbundle.SourceHostConfigurationFileSet{Files: []manifest.HostConfigurationFile{{Path: powerCutSysctlFile, Content: &content}}}
		next.Spec.Nodes[i].HostConfiguration.FileSets = configbundle.Some(merged)
	}
	data, err := yaml.Marshal(next)
	if err != nil {
		return "", err
	}
	sourcePath := filepath.Join(dir, label+"-cluster.yaml")
	if err := os.WriteFile(sourcePath, data, 0o600); err != nil {
		return "", err
	}
	configPath := filepath.Join(dir, label+".katlcfg")
	if _, err := configbundle.WriteArchive(configPath, configbundle.BuildRequest{SourcePath: sourcePath, Planning: planning}); err != nil {
		return "", err
	}
	return configPath, nil
}

// assertPowerCutRecovery waits for the node to answer after a power cut and
// checks that it booted the generation boot selection names. It returns that
// generation.
func assertPowerCutRecovery(ctx context.Context, katlctl katlctlRunner, dir, label string, node vmtest.RunningInstalledRuntimeNode, beforeBootID, beforeGeneration, wantSysctl string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, powerCutRecovery)
	defer cancel()

	var lastErr error
	for {
		probe, probeCancel := context.WithTimeout(ctx, 20*time.Second)
		_, stderr, err := runProofKatlctl(probe, katlctl, dir, label+"-status", "node", "status", node.Name, "-o", "json")
		probeCancel()
		if err == nil {
			break
		}
		lastErr = fmt.Errorf("%w: %s", err, stderr)
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%s: node status did not answer after power cut: %w", label, lastErr)
		case <-time.After(5 * time.Second):
		}
	}

	// Boot health must have passed on this boot, not only in the persisted record.
	client, err := dialNodeAgentUntil(ctx, node)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	defer client.Close()
	if err := vmtest.NewGuestControl(node.Result, client).WaitBootComplete(ctx); err != nil {
		return "", fmt.Errorf("%s: boot health after power cut: %w", label, err)
	}

	after, err := nodeBootID(ctx, node)
	if err != nil {
		return "", err
	}
	if after == "" || after == beforeBootID {
		return "", fmt.Errorf("%s: boot ID %q did not change across the power cut", label, after)
	}

	cmdline, err := readNodeFile(ctx, node, "/proc/cmdline", 16<<10)
	if err != nil {
		return "", err
	}
	match := cmdlineGeneration.FindStringSubmatch(string(cmdline))
	if match == nil {
		return "", fmt.Errorf("%s: kernel command line has no katl.generation: %s", label, cmdline)
	}
	selectionData, err := readNodeFile(ctx, node, "/var/lib/katl/boot/selection.json", 256<<10)
	if err != nil {
		return "", err
	}
	var selection struct {
		DefaultGenerationID string `json:"defaultGenerationID"`
	}
	if err := json.Unmarshal(selectionData, &selection); err != nil {
		return "", fmt.Errorf("%s: decode boot selection: %w", label, err)
	}
	booted := match[1]
	if booted != selection.DefaultGenerationID {
		return "", fmt.Errorf("%s: booted generation %s differs from boot selection default %s", label, booted, selection.DefaultGenerationID)
	}
	if strings.HasPrefix(label, "apply-") {
		spec, err := readNodeFile(ctx, node, "/var/lib/katl/generations/"+booted+"/spec.json", 1<<20)
		if err != nil {
			return "", fmt.Errorf("%s: read booted generation spec: %w", label, err)
		}
		if booted == beforeGeneration || !strings.Contains(string(spec), beforeGeneration) {
			return "", fmt.Errorf("%s: booted generation %s is not the apply's child of %s", label, booted, beforeGeneration)
		}
	} else if booted != beforeGeneration {
		return "", fmt.Errorf("%s: booted generation %s, want %s", label, booted, beforeGeneration)
	}

	got, err := nodeSysctl(ctx, node)
	if err != nil {
		return "", err
	}
	if got != wantSysctl {
		return "", fmt.Errorf("%s: %s = %q after power cut, want %q", label, powerCutSysctl, got, wantSysctl)
	}
	return booted, nil
}

// dialNodeAgentUntil dials the node's vmtest agent until it answers, since the
// guest may still be booting.
func dialNodeAgentUntil(ctx context.Context, node vmtest.RunningInstalledRuntimeNode) (*vmtest.AgentClient, error) {
	var lastErr error
	for {
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		client, err := dialNodeAgent(dialCtx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
		cancel()
		if err == nil {
			return client, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("vmtest agent did not answer after power cut: %w", lastErr)
		case <-time.After(2 * time.Second):
		}
	}
}

func nodeSysctl(ctx context.Context, node vmtest.RunningInstalledRuntimeNode) (string, error) {
	result, err := runNodeCommand(ctx, node, []string{"sysctl", "-n", powerCutSysctl}, 4<<10)
	if err != nil {
		return "", err
	}
	if result.ExitStatus != 0 {
		return "", commandErrorDetail(result)
	}
	return strings.TrimSpace(string(result.Stdout)), nil
}
