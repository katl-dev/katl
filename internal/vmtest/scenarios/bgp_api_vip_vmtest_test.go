package scenarios

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
	"github.com/katl-dev/katl/internal/installer/controlplaneendpoint"
	"github.com/katl-dev/katl/internal/installer/nodeextensionbundle"
	"github.com/katl-dev/katl/internal/vmtest"
	vmtestpb "github.com/katl-dev/katl/internal/vmtest/proto"
)

const routedEndpointProofVIP = "198.51.100.10"

func TestBIRDAndBGPAPIVIPExtensionsVMProof(t *testing.T) {
	if run, ok := bgpAPIVIPWorldRun(t); ok {
		runBGPAPIVIPProof(t, run)
		return
	}
	options := vmtest.DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run BIRD/BGP API VIP VM proof")
	}
	_ = vmtest.RequireWorld(t)
}

type bgpAPIVIPRun struct {
	WorldScenario *vmtest.WorldScenario
	Options       vmtest.Options
	Runner        vmtest.Runner
	Scenario      vmtest.Scenario
	Result        vmtest.Result
	Inputs        bgpAPIVIPInputs
}

type bgpAPIVIPInputs struct {
	ControlPlaneDisk       string                        `json:"controlPlaneDisk"`
	ControlPlaneDiskFormat string                        `json:"controlPlaneDiskFormat"`
	ControlPlaneESP        string                        `json:"controlPlaneESP"`
	ControlPlaneFixture    string                        `json:"controlPlaneFixture"`
	ControlPlaneMetadata   string                        `json:"controlPlaneMetadata"`
	ControlPlaneAddress    string                        `json:"controlPlaneAddress"`
	ControlPlaneMAC        string                        `json:"controlPlaneMAC"`
	WorkerDisk             string                        `json:"workerDisk"`
	WorkerDiskFormat       string                        `json:"workerDiskFormat"`
	WorkerESP              string                        `json:"workerESP"`
	WorkerFixture          string                        `json:"workerFixture"`
	WorkerMetadata         string                        `json:"workerMetadata"`
	WorkerAddress          string                        `json:"workerAddress"`
	WorkerMAC              string                        `json:"workerMAC"`
	RouterAddress          string                        `json:"routerAddress"`
	RouterMAC              string                        `json:"routerMAC"`
	ClientAddress          string                        `json:"clientAddress"`
	ClientMAC              string                        `json:"clientMAC"`
	WorldProvenance        multiNodeWorldProvenancePaths `json:"worldProvenance"`
}

type bgpAPIVIPExtensionProof struct {
	APIVersion        string                        `json:"apiVersion"`
	Kind              string                        `json:"kind"`
	Bundles           map[string]stagedBundleProof  `json:"bundles"`
	Config            string                        `json:"config"`
	HelperBinary      string                        `json:"helperBinary"`
	ControlPlaneProof string                        `json:"controlPlaneProof"`
	WorkerProof       string                        `json:"workerProof"`
	ControlPlaneFiles map[string]string             `json:"controlPlaneFiles,omitempty"`
	WorkerFiles       map[string]string             `json:"workerFiles,omitempty"`
	WorldProvenance   multiNodeWorldProvenancePaths `json:"worldProvenance"`
}

type stagedBundleProof struct {
	AppID                string `json:"appID"`
	PayloadVersion       string `json:"payloadVersion"`
	ArtifactVersion      string `json:"artifactVersion"`
	BundleManifestDigest string `json:"bundleManifestDigest"`
	SysextPayloadDigest  string `json:"sysextPayloadDigest"`
	BundleDir            string `json:"bundleDir"`
	SysextPath           string `json:"sysextPath"`
	ActivationPath       string `json:"activationPath"`
}

type bgpAPIVIPGuestProof struct {
	Mode                   string               `json:"mode"`
	Rejected               bool                 `json:"rejected"`
	Rejection              string               `json:"rejection"`
	AdvertisementSequence  []bool               `json:"advertisementSequence"`
	ObservedRouteExports   []observedRouteProof `json:"observedRouteExports"`
	ObservedRouteWithdraws []observedRouteProof `json:"observedRouteWithdraws"`
	RouteTable             []string             `json:"routeTable"`
}

type observedRouteProof struct {
	Peer             string   `json:"peer"`
	ExportedPrefixes []string `json:"exportedPrefixes"`
}

func bgpAPIVIPWorldRun(t *testing.T) (bgpAPIVIPRun, bool) {
	t.Helper()
	if strings.TrimSpace(os.Getenv(vmtest.WorldManifestEnv)) == "" {
		return bgpAPIVIPRun{}, false
	}
	world := vmtest.RequireWorld(t)
	repo := katlRepoRoot(t)
	kvm := vmtest.DefaultOptions().KVM
	specs := []vmtest.NodeSpec{
		{Name: "cp-1", Role: vmtest.ControlPlane},
		{Name: "worker-1", Role: vmtest.Worker},
	}
	if err := ensurePublishedRuntimeFixturesForWorld(world, repo, specs, kvm); err != nil {
		failWorldFixtureSetup(t, world, "bird-bgp-api-vip-extension-proof", err)
	}
	run, err := planBGPAPIVIPWorldRun(world, repo, kvm)
	if err != nil {
		failTwoNodeWorldSetup(t, run.WorldScenario, err)
	}
	return run, true
}

func planBGPAPIVIPWorldRun(world vmtest.World, repo string, kvm vmtest.KVMPolicy) (bgpAPIVIPRun, error) {
	scenario, err := world.PlanScenario("bird-bgp-api-vip-extension-proof")
	if err != nil {
		return bgpAPIVIPRun{}, err
	}
	run := bgpAPIVIPRun{WorldScenario: scenario}
	buildRoots := publishedRuntimeBuildRoots(world, repo)
	cp, err := vmtest.AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, buildRoots, vmtest.NodeSpec{Name: "cp-1", Role: vmtest.ControlPlane})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	worker, err := vmtest.AddPublishedInstalledRuntimeNodeFromBuildRoots(scenario, buildRoots, vmtest.NodeSpec{Name: "worker-1", Role: vmtest.Worker})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	router, err := scenario.AddNode(vmtest.NodeSpec{Name: "fabric-router", Role: vmtest.ControlPlane})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	client, err := scenario.AddNode(vmtest.NodeSpec{Name: "external-client", Role: vmtest.Worker})
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	options := vmtest.Options{
		Enabled:   true,
		StateRoot: filepath.Join(scenario.Dir, "vm-runs"),
		Keep:      vmtest.KeepFailed,
		KVM:       kvm,
		Missing:   vmtest.MissingFails,
	}
	runner := vmtest.NewRunner(options)
	vmScenario := vmtest.Scenario{Name: "bird-bgp-api-vip-extension-proof"}
	result, err := runner.Plan(vmScenario)
	if err != nil {
		_ = scenario.WriteSetupFailure(err)
		return run, err
	}
	result.Started = time.Now().UTC()
	return bgpAPIVIPRun{
		WorldScenario: scenario,
		Options:       options,
		Runner:        runner,
		Scenario:      vmScenario,
		Result:        result,
		Inputs: bgpAPIVIPInputs{
			ControlPlaneDisk:       cp.Config.Disk,
			ControlPlaneDiskFormat: string(cp.Config.DiskFormat),
			ControlPlaneESP:        cp.Config.ESPArtifacts,
			ControlPlaneFixture:    cp.Config.FixtureManifest,
			ControlPlaneMetadata:   cp.Config.NodeMetadata,
			ControlPlaneAddress:    cp.Node.Address,
			ControlPlaneMAC:        cp.Node.MACAddress,
			WorkerDisk:             worker.Config.Disk,
			WorkerDiskFormat:       string(worker.Config.DiskFormat),
			WorkerESP:              worker.Config.ESPArtifacts,
			WorkerFixture:          worker.Config.FixtureManifest,
			WorkerMetadata:         worker.Config.NodeMetadata,
			WorkerAddress:          worker.Node.Address,
			WorkerMAC:              worker.Node.MACAddress,
			RouterAddress:          router.Address,
			RouterMAC:              router.MACAddress,
			ClientAddress:          client.Address,
			ClientMAC:              client.MACAddress,
			WorldProvenance:        multiNodeWorldProvenanceForSpecs(world, repo, []vmtest.NodeSpec{{Name: "cp-1", Role: vmtest.ControlPlane}, {Name: "worker-1", Role: vmtest.Worker}}),
		},
	}, nil
}

func runBGPAPIVIPProof(t *testing.T, run bgpAPIVIPRun) {
	t.Helper()
	runner := run.Runner
	scenario := run.Scenario
	result := run.Result
	requireVMHost(t, runner, scenario, result, vmtest.HostRequirements{
		Libvirt: true,
		OVMF:    true,
		KVM:     run.Options.KVM,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	staged, err := stageBGPAPIVIPExtensionBundles(ctx, result)
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatal(err)
	}
	helper, err := buildBGPAPIVIPSmokeHelper(ctx, katlRepoRoot(t), result)
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatal(err)
	}
	cpResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	workerResult, err := vmtest.PlannedInstalledRuntimeNodeResult(result, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBGPAPIVIPProofManifest(result, run.Inputs, staged, helper, vmtest.RunningInstalledRuntimeNode{Name: "cp-1", Result: cpResult}, vmtest.RunningInstalledRuntimeNode{Name: "worker-1", Result: workerResult}, "", ""); err != nil {
		t.Fatal(err)
	}

	cpNode, err := vmtest.StartInstalledRuntimeNode(ctx, result, bgpAPIVIPNodeConfig(run, "cp-1", run.Inputs.ControlPlaneDisk, run.Inputs.ControlPlaneESP, run.Inputs.ControlPlaneFixture, run.Inputs.ControlPlaneMetadata, vmtest.DiskFormat(run.Inputs.ControlPlaneDiskFormat), run.Inputs.ControlPlaneMAC), vmtest.VMRunner{})
	if err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start cp-1 VM: %v", err)
	}
	defer stopNode(t, cpNode)

	workerNode, err := vmtest.StartInstalledRuntimeNode(ctx, result, bgpAPIVIPNodeConfig(run, "worker-1", run.Inputs.WorkerDisk, run.Inputs.WorkerESP, run.Inputs.WorkerFixture, run.Inputs.WorkerMetadata, vmtest.DiskFormat(run.Inputs.WorkerDiskFormat), run.Inputs.WorkerMAC), vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics("", cpNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start worker-1 VM: %v", err)
	}
	defer stopNode(t, workerNode)

	routerNode, err := vmtest.StartInstalledRuntimeNode(ctx, result, bgpAPIVIPNodeConfig(run, "fabric-router", run.Inputs.ControlPlaneDisk, run.Inputs.ControlPlaneESP, run.Inputs.ControlPlaneFixture, run.Inputs.ControlPlaneMetadata, vmtest.DiskFormat(run.Inputs.ControlPlaneDiskFormat), run.Inputs.RouterMAC), vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics("", cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start fabric-router VM: %v", err)
	}
	defer stopNode(t, routerNode)

	clientNode, err := vmtest.StartInstalledRuntimeNode(ctx, result, bgpAPIVIPNodeConfig(run, "external-client", run.Inputs.WorkerDisk, run.Inputs.WorkerESP, run.Inputs.WorkerFixture, run.Inputs.WorkerMetadata, vmtest.DiskFormat(run.Inputs.WorkerDiskFormat), run.Inputs.ClientMAC), vmtest.VMRunner{})
	if err != nil {
		collectTwoNodeDiagnostics("", cpNode, workerNode, routerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("start external-client VM: %v", err)
	}
	defer stopNode(t, clientNode)

	config := bgpAPIVIPConfigYAML(firstString(cpNode.Result.IPAddress, run.Inputs.ControlPlaneAddress))
	configPath := filepath.Join(result.ManifestDir, "bgp-api-vip.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	cpProof, err := runBGPAPIVIPGuestSmoke(ctx, cpNode, helper, config, "control-plane")
	if err != nil {
		collectTwoNodeDiagnostics("", cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("control-plane BGP API VIP proof: %v", err)
	}
	workerProof, err := runBGPAPIVIPGuestSmoke(ctx, workerNode, helper, config, "worker")
	if err != nil {
		collectTwoNodeDiagnostics("", cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("worker BGP API VIP proof: %v", err)
	}
	if err := assertBGPAPIVIPGuestProofs(cpProof, workerProof); err != nil {
		collectTwoNodeDiagnostics("", cpNode, workerNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatal(err)
	}
	if err := runRealRoutedEndpointProof(ctx, result, cpNode, workerNode, routerNode, clientNode, helper); err != nil {
		collectTwoNodeDiagnostics("", cpNode, workerNode, routerNode, clientNode)
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatalf("real routed endpoint proof: %v", err)
	}
	if err := writeBGPAPIVIPProofManifest(result, run.Inputs, staged, helper, cpNode, workerNode, cpProof, workerProof); err != nil {
		finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusFailed, err.Error())
		t.Fatal(err)
	}
	finishTwoNodeResult(t, runner, scenario, result, vmtest.StatusPassed, "")
}

func bgpAPIVIPNodeConfig(run bgpAPIVIPRun, name, disk, esp, fixture, metadata string, format vmtest.DiskFormat, mac string) vmtest.InstalledRuntimeNodeConfig {
	return vmtest.InstalledRuntimeNodeConfig{
		Name: name,
		Runtime: vmtest.InstalledRuntimeConfig{
			Disk:               disk,
			DiskFormat:         format,
			ESPArtifacts:       esp,
			FixtureManifest:    fixture,
			NodeMetadata:       metadata,
			RequireVMTestAgent: true,
			VM: vmtest.VMConfig{
				KVM:    run.Options.KVM,
				RAMMiB: 2048,
				CPUs:   2,
				Network: vmtest.VMNetworkConfig{
					MAC: mac,
				},
				Timeout: 8 * time.Minute,
				Agent: vmtest.AgentControlConfig{
					RequireHealth: true,
					Timeout:       30 * time.Second,
				},
			},
		},
	}
}

func stageBGPAPIVIPExtensionBundles(ctx context.Context, result vmtest.Result) (map[string]stagedBundleProof, error) {
	root := filepath.Join(result.ManifestDir, "node-extension-bundles")
	birdRoot := filepath.Join(root, nodeextensionbundle.BirdAppID)
	bird, err := nodeextensionbundle.WriteBirdFixture(nodeextensionbundle.BirdFixtureRequest{OutputDir: birdRoot})
	if err != nil {
		return nil, err
	}
	vipRoot := filepath.Join(root, nodeextensionbundle.BGPAPIVIPAppID)
	vip, err := nodeextensionbundle.WriteBGPAPIVIPFixture(nodeextensionbundle.BGPAPIVIPFixtureRequest{OutputDir: vipRoot})
	if err != nil {
		return nil, err
	}
	out := map[string]stagedBundleProof{}
	for _, source := range []struct {
		root    string
		fixture nodeextensionbundle.Fixture
	}{
		{root: birdRoot, fixture: bird},
		{root: vipRoot, fixture: vip},
	} {
		server := httptest.NewTLSServer(http.FileServer(http.Dir(source.root)))
		staged, err := nodeextensionbundle.FetchAndStage(ctx, nodeextensionbundle.Request{
			Source:           server.URL,
			Ref:              nodeExtensionFixtureRef(source.fixture),
			CacheDir:         filepath.Join(result.ManifestDir, "staged-node-extensions"),
			RuntimeInterface: "katl-runtime-1",
			Architecture:     "x86_64",
			Client:           server.Client(),
		})
		server.Close()
		if err != nil {
			return nil, err
		}
		out[staged.AppID] = stagedBundleProof{
			AppID:                staged.AppID,
			PayloadVersion:       staged.PayloadVersion,
			ArtifactVersion:      staged.ArtifactVersion,
			BundleManifestDigest: staged.BundleManifestDigest,
			SysextPayloadDigest:  staged.SysextPayloadDigest,
			BundleDir:            staged.BundleDir,
			SysextPath:           staged.SysextPath,
			ActivationPath:       staged.ExtensionRef.ActivationPath,
		}
	}
	for _, appID := range []string{nodeextensionbundle.BirdAppID, nodeextensionbundle.BGPAPIVIPAppID} {
		if _, ok := out[appID]; !ok {
			return nil, fmt.Errorf("staged extension bundles missing %s", appID)
		}
	}
	return out, nil
}

func nodeExtensionFixtureRef(fixture nodeextensionbundle.Fixture) string {
	var index nodeextensionbundle.Index
	data, err := os.ReadFile(fixture.IndexPath)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(data, &index); err != nil {
		panic(err)
	}
	entry := index.Entries[0]
	return entry.AppID + "/" + entry.PayloadVersion + "@" + entry.BundleManifestDigest
}

func buildBGPAPIVIPSmokeHelper(ctx context.Context, repo string, result vmtest.Result) (string, error) {
	path := filepath.Join(result.ManifestDir, "bgp-api-vip-smoke")
	cmd := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-trimpath", "-ldflags", "-s -w", "-o", path, "./internal/vmtest/testcmd/bgp-api-vip-smoke")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOCACHE="+filepath.Join(result.RunDir, "go-cache"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build bgp-api-vip-smoke helper: %w\n%s", err, output)
	}
	return path, nil
}

func runBGPAPIVIPGuestSmoke(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, helper string, config string, mode string) (string, error) {
	root := "/var/lib/katl/test-artifacts/bgp-api-vip"
	binary := root + "/bin/bgp-api-vip-smoke"
	configPath := root + "/" + mode + "/config.yaml"
	outputDir := root + "/" + mode + "/evidence"
	data, err := os.ReadFile(helper)
	if err != nil {
		return "", err
	}
	if err := writeNodeFileChunked(ctx, node, binary, data, 0o755); err != nil {
		return "", err
	}
	if err := writeNodeFile(ctx, node, configPath, []byte(config), 0o644, false); err != nil {
		return "", err
	}
	opCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client, err := vmtest.DialAgent(opCtx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		return "", err
	}
	defer client.Close()
	guest := vmtest.NewGuestControl(node.Result, client)
	if _, err := guest.RunCommand(opCtx, vmtest.GuestCommandRequest{
		Name: "bgp-api-vip-smoke-" + mode,
		Argv: []string{binary},
		Environment: []*vmtestpb.EnvVar{
			{Name: "KATL_BGP_API_VIP_CONFIG", Value: configPath},
			{Name: "KATL_BGP_API_VIP_SMOKE_MODE", Value: mode},
			{Name: "KATL_BGP_API_VIP_SMOKE_OUTPUT", Value: outputDir},
		},
		StdoutLimit: 32 << 10,
		StderrLimit: 32 << 10,
		Timeout:     90 * time.Second,
	}); err != nil {
		return "", err
	}
	proofPath := outputDir + "/proof.json"
	proof, err := readNodeFile(ctx, node, proofPath, 256<<10)
	if err != nil {
		return "", err
	}
	hostPath := filepath.Join(node.Result.Artifacts.GuestDir, "bgp-api-vip", mode+"-proof.json")
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(hostPath, proof, 0o644); err != nil {
		return "", err
	}
	for _, guestPath := range []string{
		outputDir + "/status-live.json",
		outputDir + "/status-operation.json",
		outputDir + "/rendered/etc/katl/apps/bird/bird.conf",
		outputDir + "/rendered/etc/katl/apps/bgp-api-vip/config.yaml",
	} {
		data, err := readNodeFile(ctx, node, guestPath, 512<<10)
		if err != nil {
			continue
		}
		target := filepath.Join(node.Result.Artifacts.GuestDir, "bgp-api-vip", mode, strings.TrimPrefix(guestPath, root+"/"))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return "", err
		}
	}
	return hostPath, nil
}

func assertBGPAPIVIPGuestProofs(cpProofPath, workerProofPath string) error {
	var cp bgpAPIVIPGuestProof
	if err := readProofJSON(cpProofPath, &cp); err != nil {
		return err
	}
	if cp.Mode != "control-plane" || len(cp.ObservedRouteExports) != 1 || len(cp.ObservedRouteExports[0].ExportedPrefixes) != 1 || cp.ObservedRouteExports[0].ExportedPrefixes[0] != "10.40.0.10/32" {
		return fmt.Errorf("control-plane proof = %#v", cp)
	}
	if len(cp.RouteTable) != 0 {
		return fmt.Errorf("control-plane route table = %v, want withdrawn", cp.RouteTable)
	}
	var worker bgpAPIVIPGuestProof
	if err := readProofJSON(workerProofPath, &worker); err != nil {
		return err
	}
	if worker.Mode != "worker" || !worker.Rejected || !strings.Contains(worker.Rejection, "cannot advertise BGP API VIP") {
		return fmt.Errorf("worker proof = %#v", worker)
	}
	return nil
}

type routedEndpointVMProof struct {
	APIVersion               string   `json:"apiVersion"`
	Kind                     string   `json:"kind"`
	VIP                      string   `json:"vip"`
	ControlPlane             string   `json:"controlPlane"`
	FabricRouter             string   `json:"fabricRouter"`
	ExternalClient           string   `json:"externalClient"`
	Worker                   string   `json:"worker"`
	InitialBirdInactive      bool     `json:"initialBirdInactive"`
	WorkerArtifactAbsent     bool     `json:"workerArtifactAbsent"`
	RouteAdvertised          bool     `json:"routeAdvertised"`
	ExternalReadyzReachable  bool     `json:"externalReadyzReachable"`
	APIFailureWithdrawn      bool     `json:"apiFailureWithdrawn"`
	APIRecoveryReadvertised  bool     `json:"apiRecoveryReadvertised"`
	ControllerKillWithdrawn  bool     `json:"controllerKillWithdrawn"`
	RoutingDaemonKillRemoved bool     `json:"routingDaemonKillRemoved"`
	Checks                   []string `json:"checks"`
}

func runRealRoutedEndpointProof(ctx context.Context, result vmtest.Result, cp, worker, router, client vmtest.RunningInstalledRuntimeNode, helper string) error {
	proof := routedEndpointVMProof{
		APIVersion:     "katl.dev/v1alpha1",
		Kind:           "RoutedControlPlaneEndpointVMProof",
		VIP:            routedEndpointProofVIP + "/32",
		ControlPlane:   cp.Result.IPAddress,
		FabricRouter:   router.Result.IPAddress,
		ExternalClient: client.Result.IPAddress,
		Worker:         worker.Result.IPAddress,
	}
	writeProof := func() {
		data, _ := json.MarshalIndent(proof, "", "  ")
		_ = os.WriteFile(filepath.Join(result.ManifestDir, "routed-control-plane-endpoint-proof.json"), append(data, '\n'), 0o644)
	}
	defer writeProof()

	if err := assertGuestCommand(ctx, worker, []string{"test", "!", "-e", "/var/lib/katl/artifacts/katlos-image/katl-endpoint-advertiser.raw"}); err != nil {
		return fmt.Errorf("worker retained endpoint advertiser artifact: %w", err)
	}
	proof.WorkerArtifactAbsent = true
	proof.Checks = append(proof.Checks, "worker has no endpoint advertiser payload")

	for _, node := range []vmtest.RunningInstalledRuntimeNode{cp, router} {
		if err := activateRetainedEndpointSysext(ctx, node); err != nil {
			return fmt.Errorf("activate endpoint sysext on %s: %w", node.Name, err)
		}
	}
	if err := assertGuestCommand(ctx, cp, []string{"systemctl", "start", "katl-app-bird.service"}); err != nil {
		return fmt.Errorf("ask conditioned BIRD unit to start without config: %w", err)
	}
	if active, err := guestUnitActive(ctx, cp, "katl-app-bird.service"); err != nil {
		return err
	} else if active {
		return fmt.Errorf("BIRD became active without managed VIP configuration")
	}
	proof.InitialBirdInactive = true
	proof.Checks = append(proof.Checks, "BIRD remains inactive without the managed-advertisement marker")

	if err := configureFabricRouter(ctx, router, []string{cp.Result.IPAddress}); err != nil {
		return err
	}
	if err := assertGuestCommand(ctx, router, []string{"sysctl", "-w", "net.ipv4.ip_forward=1"}); err != nil {
		return fmt.Errorf("enable fabric router forwarding: %w", err)
	}
	if err := assertGuestCommand(ctx, client, []string{"ip", "route", "replace", routedEndpointProofVIP + "/32", "via", router.Result.IPAddress}); err != nil {
		return fmt.Errorf("install external client VIP route: %w", err)
	}

	caPEM, certPEM, keyPEM, err := routedEndpointTLSFixture(net.ParseIP(routedEndpointProofVIP))
	if err != nil {
		return err
	}
	const fixtureRoot = "/var/lib/katl/test-artifacts/routed-endpoint"
	for _, target := range []struct {
		node vmtest.RunningInstalledRuntimeNode
		path string
		data []byte
		mode uint32
	}{
		{cp, fixtureRoot + "/ca.crt", caPEM, 0o644},
		{cp, fixtureRoot + "/server.crt", certPEM, 0o644},
		{cp, fixtureRoot + "/server.key", keyPEM, 0o600},
		{client, fixtureRoot + "/ca.crt", caPEM, 0o644},
	} {
		if err := writeNodeFile(ctx, target.node, target.path, target.data, target.mode, target.mode == 0o600); err != nil {
			return err
		}
	}
	if err := assertGuestCommand(ctx, cp, []string{
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
		"/usr/bin/install", "-D", "-m", "0644", fixtureRoot + "/ca.crt", "/etc/kubernetes/pki/ca.crt",
	}); err != nil {
		return fmt.Errorf("install kubeadm-compatible API CA: %w", err)
	}
	helperData, err := os.ReadFile(helper)
	if err != nil {
		return err
	}
	for _, node := range []vmtest.RunningInstalledRuntimeNode{cp, client} {
		if err := writeNodeFileChunked(ctx, node, fixtureRoot+"/bgp-api-vip-smoke", helperData, 0o755); err != nil {
			return err
		}
	}

	endpointPlan, err := controlPlaneEndpointVMFiles(router.Result.IPAddress)
	if err != nil {
		return err
	}
	if err := activateEndpointConfext(ctx, cp, fixtureRoot, endpointPlan); err != nil {
		return err
	}
	if err := createGuestDummyVIP(ctx, cp); err != nil {
		return err
	}
	if err := startReadyzFixture(ctx, cp, fixtureRoot); err != nil {
		return err
	}
	defer func() {
		_, _ = runNodeCommand(context.Background(), cp, []string{"systemctl", "stop", "katl-vmtest-readyz.service"}, 8<<10)
	}()
	if err := assertGuestCommand(ctx, cp, []string{"systemctl", "daemon-reload"}); err != nil {
		return err
	}
	if err := assertGuestCommand(ctx, cp, []string{"systemctl", "start", "katl-app-bgp-api-vip.service"}); err != nil {
		return fmt.Errorf("start production endpoint controller: %w", err)
	}

	if _, err := waitForRouterRoute(ctx, router, routedEndpointProofVIP, true, 30*time.Second); err != nil {
		return err
	}
	proof.RouteAdvertised = true
	proof.Checks = append(proof.Checks, "fabric router learned the healthy API /32 from production BIRD")
	if err := probeReadyzFromClient(ctx, client, fixtureRoot); err != nil {
		return err
	}
	proof.ExternalReadyzReachable = true
	proof.Checks = append(proof.Checks, "external client reached TLS /readyz through the fabric router")

	if err := assertGuestCommand(ctx, cp, []string{"systemctl", "stop", "katl-vmtest-readyz.service"}); err != nil {
		return fmt.Errorf("stop API readiness fixture: %w", err)
	}
	if _, err := waitForRouterRoute(ctx, router, routedEndpointProofVIP, false, 20*time.Second); err != nil {
		return fmt.Errorf("API failure did not withdraw route: %w", err)
	}
	proof.APIFailureWithdrawn = true
	proof.Checks = append(proof.Checks, "local API failure withdrew only the API route")
	if err := startReadyzFixture(ctx, cp, fixtureRoot); err != nil {
		return err
	}
	if _, err := waitForRouterRoute(ctx, router, routedEndpointProofVIP, true, 30*time.Second); err != nil {
		return fmt.Errorf("API recovery did not readvertise route: %w", err)
	}
	proof.APIRecoveryReadvertised = true

	if err := setVMTestRestartPolicy(ctx, cp, "katl-app-bgp-api-vip.service", "no"); err != nil {
		return err
	}
	if err := assertGuestCommand(ctx, cp, []string{"systemctl", "kill", "--kill-whom=main", "--signal=SIGKILL", "katl-app-bgp-api-vip.service"}); err != nil {
		return fmt.Errorf("kill endpoint controller: %w", err)
	}
	if _, err := waitForRouterRoute(ctx, router, routedEndpointProofVIP, false, 20*time.Second); err != nil {
		return fmt.Errorf("ungraceful controller death did not withdraw route: %w", err)
	}
	proof.ControllerKillWithdrawn = true
	proof.Checks = append(proof.Checks, "ungraceful controller death failed closed")
	if err := setVMTestRestartPolicy(ctx, cp, "katl-app-bgp-api-vip.service", "always"); err != nil {
		return err
	}
	if err := assertGuestCommand(ctx, cp, []string{"systemctl", "reset-failed", "katl-app-bgp-api-vip.service"}); err != nil {
		return err
	}
	if err := assertGuestCommand(ctx, cp, []string{"systemctl", "start", "katl-app-bgp-api-vip.service"}); err != nil {
		return err
	}
	if _, err := waitForRouterRoute(ctx, router, routedEndpointProofVIP, true, 30*time.Second); err != nil {
		return err
	}

	if err := setVMTestRestartPolicy(ctx, cp, "katl-app-bird.service", "no"); err != nil {
		return err
	}
	if err := assertGuestCommand(ctx, cp, []string{"systemctl", "kill", "--kill-whom=main", "--signal=SIGKILL", "katl-app-bird.service"}); err != nil {
		return fmt.Errorf("kill routing daemon: %w", err)
	}
	if _, err := waitForRouterRoute(ctx, router, routedEndpointProofVIP, false, 20*time.Second); err != nil {
		return fmt.Errorf("routing daemon death did not remove route: %w", err)
	}
	proof.RoutingDaemonKillRemoved = true
	proof.Checks = append(proof.Checks, "routing-daemon death removed the fabric route")
	return nil
}

func activateRetainedEndpointSysext(ctx context.Context, node vmtest.RunningInstalledRuntimeNode) error {
	const artifact = "/var/lib/katl/artifacts/katlos-image/katl-endpoint-advertiser.raw"
	if err := assertGuestCommand(ctx, node, []string{"test", "-f", artifact}); err != nil {
		return fmt.Errorf("retained endpoint artifact is missing: %w", err)
	}
	for _, argv := range [][]string{
		{"install", "-d", "/run/extensions"},
		{"systemd-run", "--quiet", "--wait", "--collect", "--pipe", "/usr/bin/ln", "-sf", artifact, "/run/extensions/katl-endpoint-advertiser.raw"},
		{"systemd-run", "--quiet", "--wait", "--collect", "--pipe", "/usr/bin/systemd-sysext", "refresh"},
		{"systemctl", "daemon-reload"},
	} {
		if err := assertGuestCommand(ctx, node, argv); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		}
	}
	return nil
}

func controlPlaneEndpointVMFiles(routerAddress string) (bgpapivip.Plan, error) {
	intent, err := controlplaneendpoint.Normalize(controlplaneendpoint.Config{
		Host: routedEndpointProofVIP,
		Advertisement: &controlplaneendpoint.Advertisement{
			VIP: routedEndpointProofVIP,
			BGP: &controlplaneendpoint.BGP{
				LocalASN: 64512,
				Peers:    []controlplaneendpoint.Peer{{Address: routerAddress, ASN: 64500}},
			},
		},
	})
	if err != nil {
		return bgpapivip.Plan{}, err
	}
	config, err := bgpapivip.FromControlPlaneEndpoint(intent)
	if err != nil {
		return bgpapivip.Plan{}, err
	}
	return bgpapivip.RenderNativeEtcFiles(bgpapivip.RenderRequest{NodeRole: "control-plane", Config: config})
}

func configureFabricRouter(ctx context.Context, router vmtest.RunningInstalledRuntimeNode, peers []string) error {
	var config strings.Builder
	fmt.Fprintf(&config, "router id %s;\n\nprotocol device {}\n\n", router.Result.IPAddress)
	config.WriteString("protocol kernel katl_kernel {\n  ipv4 { import none; export all; };\n  merge paths on limit 16;\n}\n\n")
	for index, peer := range peers {
		fmt.Fprintf(&config, "protocol bgp katl_cp_%d {\n  local as 64500;\n  neighbor %s as 64512;\n  ipv4 { import all; export none; };\n}\n\n", index+1, peer)
	}
	const path = "/var/lib/katl/test-artifacts/routed-endpoint/fabric-router.conf"
	if err := writeNodeFile(ctx, router, path, []byte(config.String()), 0o644, false); err != nil {
		return err
	}
	if err := assertGuestCommand(ctx, router, []string{"install", "-d", "/run/katl-fabric-router"}); err != nil {
		return err
	}
	if err := assertGuestCommand(ctx, router, []string{
		"systemd-run", "--quiet", "--collect", "--unit=katl-vmtest-fabric-router.service", "--property=Restart=on-failure",
		"/usr/bin/bird", "-f", "-c", path, "-s", "/run/katl-fabric-router/bird.ctl",
	}); err != nil {
		return fmt.Errorf("start fabric BIRD: %w", err)
	}
	return nil
}

func installStagedNodeFile(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, stagingRoot, target string, data []byte, mode uint32, sensitive bool) error {
	staged := stagingRoot + "/privileged" + filepath.Clean(target)
	if err := writeNodeFile(ctx, node, staged, data, mode, sensitive); err != nil {
		return err
	}
	if err := assertGuestCommand(ctx, node, []string{
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
		"/usr/bin/install", "-D", "-m", fmt.Sprintf("%04o", mode), staged, target,
	}); err != nil {
		return fmt.Errorf("install %s: %w", target, err)
	}
	return nil
}

func activateEndpointConfext(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, stagingRoot string, plan bgpapivip.Plan) error {
	const (
		name = "katl-endpoint-vmtest"
		root = "/run/confexts/" + name
	)
	for _, file := range plan.Files {
		switch file.Path {
		case bgpapivip.ConfigPath, bgpapivip.BirdConfigPath, bgpapivip.AdvertisementEnabledPath:
			target := root + file.Path
			if err := installStagedNodeFile(ctx, node, stagingRoot, target, []byte(file.Content), uint32(file.Mode.Perm()), false); err != nil {
				return err
			}
		}
	}
	releasePath := root + "/etc/extension-release.d/extension-release." + name
	if err := installStagedNodeFile(ctx, node, stagingRoot, releasePath, []byte("ID=katlos\nCONFEXT_LEVEL=1\n"), 0o644, false); err != nil {
		return err
	}
	if err := assertGuestCommand(ctx, node, []string{
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe", "/usr/bin/systemd-confext", "refresh",
	}); err != nil {
		return fmt.Errorf("activate endpoint confext: %w", err)
	}
	return nil
}

func createGuestDummyVIP(ctx context.Context, node vmtest.RunningInstalledRuntimeNode) error {
	for _, argv := range [][]string{
		{"ip", "link", "add", "katl-api", "type", "dummy"},
		{"ip", "address", "add", routedEndpointProofVIP + "/32", "dev", "katl-api"},
		{"ip", "link", "set", "katl-api", "up"},
	} {
		if err := assertGuestCommand(ctx, node, argv); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		}
	}
	return nil
}

func startReadyzFixture(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, root string) error {
	_, _ = runNodeCommand(ctx, node, []string{"systemctl", "stop", "katl-vmtest-readyz.service"}, 8<<10)
	return assertGuestCommand(ctx, node, []string{
		"systemd-run", "--quiet", "--collect", "--unit=katl-vmtest-readyz.service",
		root + "/bgp-api-vip-smoke", "serve-readyz",
		"--listen", routedEndpointProofVIP + ":6443", "--cert", root + "/server.crt", "--key", root + "/server.key",
	})
}

func probeReadyzFromClient(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, root string) error {
	result, err := runNodeCommand(ctx, node, []string{
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
		root + "/bgp-api-vip-smoke", "probe-readyz",
		"--url", "https://" + routedEndpointProofVIP + ":6443/readyz", "--ca", root + "/ca.crt", "--server-name", routedEndpointProofVIP,
	}, 16<<10)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 || strings.TrimSpace(string(result.Stdout)) != "ok" {
		return fmt.Errorf("external readyz probe: %w", commandErrorDetail(result))
	}
	return nil
}

func waitForRouterRoute(ctx context.Context, router vmtest.RunningInstalledRuntimeNode, prefix string, present bool, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		result, err := runNodeCommand(ctx, router, []string{
			"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
			"/usr/bin/birdc", "-s", "/run/katl-fabric-router/bird.ctl", "show", "route", "for", prefix, "all",
		}, 32<<10)
		if err == nil {
			last = string(result.Stdout) + string(result.Stderr)
			hasRoute := result.ExitStatus == 0 && strings.Contains(last, prefix)
			if hasRoute == present {
				return last, nil
			}
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return last, fmt.Errorf("fabric route %s presence=%t not observed: %s", prefix, present, strings.TrimSpace(last))
}

func guestUnitActive(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, unit string) (bool, error) {
	result, err := runNodeCommand(ctx, node, []string{"systemctl", "is-active", "--quiet", unit}, 8<<10)
	if err != nil {
		return false, err
	}
	return result.ExitStatus == 0, nil
}

func assertGuestCommand(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, argv []string) error {
	result, err := runNodeCommand(ctx, node, argv, 32<<10)
	if err != nil {
		return err
	}
	if result.ExitStatus != 0 {
		return commandErrorDetail(result)
	}
	return nil
}

func setVMTestRestartPolicy(ctx context.Context, node vmtest.RunningInstalledRuntimeNode, unit, restart string) error {
	root := "/var/lib/katl/test-artifacts/routed-endpoint"
	dropIn := root + "/" + strings.ReplaceAll(unit, ".", "-") + "-restart.conf"
	if err := writeNodeFile(ctx, node, dropIn, []byte("[Service]\nRestart="+restart+"\n"), 0o644, false); err != nil {
		return err
	}
	target := "/run/systemd/system/" + unit + ".d/90-vmtest-restart.conf"
	if err := assertGuestCommand(ctx, node, []string{
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe", "/usr/bin/install", "-D", "-m", "0644", dropIn, target,
	}); err != nil {
		return err
	}
	return assertGuestCommand(ctx, node, []string{"systemctl", "daemon-reload"})
}

func routedEndpointTLSFixture(vip net.IP) ([]byte, []byte, []byte, error) {
	now := time.Now().UTC()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, err
	}
	caTemplate := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "katl routed endpoint VM CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, err
	}
	serverTemplate := x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: routedEndpointProofVIP},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{vip},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, &serverTemplate, &caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
	return caPEM, certPEM, keyPEM, nil
}

func readProofJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func bgpAPIVIPConfigYAML(sourceAddress string) string {
	sourceAddress = strings.TrimSpace(sourceAddress)
	if sourceAddress == "" {
		sourceAddress = "192.0.2.10"
	}
	return fmt.Sprintf(`apiVersion: %s
kind: %s
spec:
  endpoint:
    host: api.home.example
    vip: 10.40.0.10/32
  vipInterface:
    kind: dummy
    name: katl-api0
    mtu: 1500
  routing:
    routerID: %s
    localASN: 64512
    sourceAddress: %s
    sourceInterface: enp1s0
    exportPolicy:
      communities:
        - "64512:100"
      localPreference: 100
  devHostPeers:
    - name: dev-host
      address: 10.0.0.1
      asn: 64500
      allowedExportPrefixes:
        - 10.40.0.10/32
`, bgpapivip.APIVersion, bgpapivip.Kind, sourceAddress, sourceAddress)
}

func writeBGPAPIVIPProofManifest(result vmtest.Result, inputs bgpAPIVIPInputs, bundles map[string]stagedBundleProof, helper string, cpNode, workerNode vmtest.RunningInstalledRuntimeNode, cpProof, workerProof string) error {
	proof := bgpAPIVIPExtensionProof{
		APIVersion:        vmtest.WorldAPIVersion,
		Kind:              "BIRDAndBGPAPIVIPExtensionProof",
		Bundles:           bundles,
		Config:            filepath.Join(result.ManifestDir, "bgp-api-vip.yaml"),
		HelperBinary:      helper,
		ControlPlaneProof: cpProof,
		WorkerProof:       workerProof,
		WorldProvenance:   inputs.WorldProvenance,
		ControlPlaneFiles: map[string]string{
			"result":     cpNode.Result.Artifacts.Result,
			"serial":     cpNode.Result.Artifacts.RuntimeSerial,
			"transcript": cpNode.Result.Artifacts.VSockTranscript,
		},
		WorkerFiles: map[string]string{
			"result":     workerNode.Result.Artifacts.Result,
			"serial":     workerNode.Result.Artifacts.RuntimeSerial,
			"transcript": workerNode.Result.Artifacts.VSockTranscript,
		},
	}
	data, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(result.ManifestDir, "bird-bgp-api-vip-extension-proof.json"), append(data, '\n'), 0o644)
}
