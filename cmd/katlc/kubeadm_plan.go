package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/katl-dev/katl/internal/installer/kubeadmconfig"
	"github.com/katl-dev/katl/internal/installer/kubeadmplan"
)

const (
	kubeadmPlanExitNoChanges      = 0
	kubeadmPlanExitActionRequired = 2
	kubeadmPlanExitManual         = 3
	kubeadmPlanExitCollection     = 4
)

type commandExitError struct {
	code    int
	message string
}

func (e commandExitError) Error() string { return e.message }

type kubeadmPlanOutput struct {
	APIVersion     string                     `json:"apiVersion"`
	Kind           string                     `json:"kind"`
	ConfigName     string                     `json:"configName"`
	DesiredPath    string                     `json:"desiredPath"`
	Classification kubeadmplan.Classification `json:"classification"`
	ExitCode       int                        `json:"exitCode"`
	AutomaticApply bool                       `json:"automaticApply"`
	NextActions    []string                   `json:"nextActions"`
	Changes        []kubeadmplan.Change       `json:"changes,omitempty"`
}

func runKubeadm(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "plan" {
		return fmt.Errorf("unsupported kubeadm command %q", strings.Join(args, " "))
	}
	flags := flag.NewFlagSet("katlc kubeadm plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "/", "runtime root")
	name := flags.String("name", "", "selected KubeadmConfig name; defaults from active node metadata")
	liveDir := flags.String("live-state-dir", "", "read-only live-state snapshot directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	_ = ctx
	selected, version, err := selectedKubeadmConfig(*root, *name)
	if err != nil {
		return writeKubeadmCollectionFailure(stdout, selected, err)
	}
	liveRoot := *root
	if strings.TrimSpace(*liveDir) != "" {
		liveRoot = *liveDir
	}
	live, err := collectLocalKubeadmState(liveRoot)
	if err != nil {
		return writeKubeadmCollectionFailure(stdout, selected, err)
	}
	desiredPath := kubeadmconfig.ConfigPath(selected)
	patchesDir := kubeadmconfig.PatchesDir(selected)
	if _, statErr := os.Stat(filepath.Join(filepath.Clean(*root), strings.TrimPrefix(patchesDir, "/"))); errors.Is(statErr, os.ErrNotExist) {
		patchesDir = ""
	} else if statErr != nil {
		return writeKubeadmCollectionFailure(stdout, selected, statErr)
	}
	plan, err := kubeadmplan.PlanDesiredLive(kubeadmplan.Request{
		FS:                        os.DirFS(filepath.Clean(*root)),
		ConfigPath:                desiredPath,
		PatchesDir:                patchesDir,
		SelectedKubernetesVersion: version,
		Live:                      live,
	})
	if err != nil {
		return writeKubeadmCollectionFailure(stdout, selected, err)
	}
	code, actions := kubeadmPlanDisposition(plan.Classification)
	output := kubeadmPlanOutput{
		APIVersion:     "katl.dev/v1alpha1",
		Kind:           "KubeadmPlan",
		ConfigName:     selected,
		DesiredPath:    desiredPath,
		Classification: plan.Classification,
		ExitCode:       code,
		AutomaticApply: false,
		NextActions:    actions,
		Changes:        plan.Changes,
	}
	if err := writeKubeadmPlan(stdout, output); err != nil {
		return err
	}
	if code != 0 {
		return commandExitError{code: code}
	}
	return nil
}

func selectedKubeadmConfig(root, override string) (string, string, error) {
	if override != "" {
		if err := validateConfigName(override); err != nil {
			return "", "", err
		}
	}
	data, err := os.ReadFile(filepath.Join(filepath.Clean(root), "etc/katl/node.json"))
	if err != nil {
		if override != "" {
			return override, "", nil
		}
		return "", "", fmt.Errorf("read active node metadata: %w", err)
	}
	var node struct {
		Kubeadm struct {
			ConfigRef string `json:"configRef"`
		} `json:"kubeadm"`
		Kubernetes struct {
			PayloadVersion string `json:"payloadVersion"`
		} `json:"kubernetes"`
	}
	if err := json.Unmarshal(data, &node); err != nil {
		return "", "", fmt.Errorf("decode active node metadata: %w", err)
	}
	selected := override
	if selected == "" {
		selected = node.Kubeadm.ConfigRef
	}
	if err := validateConfigName(selected); err != nil {
		return "", "", err
	}
	return selected, strings.TrimSpace(node.Kubernetes.PayloadVersion), nil
}

func validateConfigName(name string) error {
	if strings.TrimSpace(name) == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\\`) {
		return fmt.Errorf("KubeadmConfig name %q is invalid", name)
	}
	return nil
}

func collectLocalKubeadmState(root string) (kubeadmplan.LiveSnapshot, error) {
	read := func(path string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(filepath.Clean(root), path))
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	var live kubeadmplan.LiveSnapshot
	configMap := &kubeadmplan.KubeadmConfigMapSnapshot{}
	var err error
	if configMap.ClusterConfiguration, err = read("kubeadm-config/ClusterConfiguration.yaml"); err != nil {
		return live, err
	}
	if configMap.InitConfiguration, err = read("kubeadm-config/InitConfiguration.yaml"); err != nil {
		return live, err
	}
	if configMap.JoinConfiguration, err = read("kubeadm-config/JoinConfiguration.yaml"); err != nil {
		return live, err
	}
	if len(configMap.ClusterConfiguration)+len(configMap.InitConfiguration)+len(configMap.JoinConfiguration) > 0 {
		live.KubeadmConfigMap = configMap
	}
	if live.KubeletConfigMap, err = read("kubelet-config.yaml"); err != nil {
		return live, err
	}
	if live.KubeletConfig, err = read("var/lib/kubelet/config.yaml"); err != nil {
		return live, err
	}
	if live.KubeadmFlagsEnv, err = read("var/lib/kubelet/kubeadm-flags.env"); err != nil {
		return live, err
	}
	live.StaticPodManifests = map[string][]byte{}
	entries, err := os.ReadDir(filepath.Join(filepath.Clean(root), "etc/kubernetes/manifests"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return live, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, readErr := read(filepath.Join("etc/kubernetes/manifests", entry.Name()))
		if readErr != nil {
			return live, readErr
		}
		live.StaticPodManifests[entry.Name()] = data
	}
	if len(live.StaticPodManifests) == 0 {
		live.StaticPodManifests = nil
	}
	return live, nil
}

func kubeadmPlanDisposition(classification kubeadmplan.Classification) (int, []string) {
	switch classification {
	case kubeadmplan.NoOp:
		return kubeadmPlanExitNoChanges, []string{"none; desired and live kubeadm state match"}
	case kubeadmplan.UnsupportedManualIntervention:
		return kubeadmPlanExitManual, []string{"review unsupported differences manually; katlc will not apply them"}
	case kubeadmplan.ExplicitUpgradeNeeded:
		return kubeadmPlanExitActionRequired, []string{"run an explicit KatlOS Kubernetes upgrade operation after review"}
	case kubeadmplan.BootstrapNeeded:
		return kubeadmPlanExitActionRequired, []string{"run the explicit kubeadm bootstrap operation appropriate for this node"}
	default:
		return kubeadmPlanExitActionRequired, []string{"run an explicit kubeadm control-plane configuration operation after review"}
	}
}

func writeKubeadmCollectionFailure(stdout io.Writer, name string, cause error) error {
	output := struct {
		APIVersion  string   `json:"apiVersion"`
		Kind        string   `json:"kind"`
		ConfigName  string   `json:"configName,omitempty"`
		ExitCode    int      `json:"exitCode"`
		Diagnostics []string `json:"diagnostics"`
	}{"katl.dev/v1alpha1", "KubeadmPlanFailure", name, kubeadmPlanExitCollection, []string{"unable to collect or validate kubeadm planning inputs"}}
	if err := json.NewEncoder(stdout).Encode(output); err != nil {
		return err
	}
	return commandExitError{code: kubeadmPlanExitCollection, message: fmt.Sprintf("kubeadm plan collection failed: %v", cause)}
}

func writeKubeadmPlan(out io.Writer, output kubeadmPlanOutput) error {
	sort.Slice(output.Changes, func(i, j int) bool { return output.Changes[i].Field < output.Changes[j].Field })
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}
