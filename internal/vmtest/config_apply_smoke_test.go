package vmtest

import (
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/operation"
	agent "github.com/katl-dev/katl/internal/katlc/agent"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlc/transport"
	"github.com/katl-dev/katl/internal/managementidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

//go:embed testdata/config-apply/50-vmtest-stall.conf
var extensionActivationStallDropIn string

func TestInstalledRuntimeConfigApplyModesSmoke(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("set -katl.vmtest.run or KATL_VMTEST_RUN=1 to run installed runtime config apply smoke")
	}
	runner := NewRunner(options)
	runtime := InstalledRuntimeConfig{}
	var plannedAddress string
	var worldScenario *WorldScenario
	if worldRun, ok := installedRuntimeWorldRunFor(t, "installed-runtime-config-apply-modes", NodeSpec{Name: "cp-1", Role: ControlPlane}); ok {
		runner = worldRun.Runner
		runtime = worldRun.Config
		plannedAddress = worldRun.Node.Address
		worldScenario = worldRun.Scenario
	} else {
		_ = RequireWorld(t)
	}
	scenario := Scenario{
		Name:  "installed-runtime-config-apply-modes",
		Disks: []DiskFixture{ExtraDisk("data", "raw", "1G"), ExtraDisk("scratch", "raw", "512M")},
	}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	result = requirePlannedVMHost(t, runner, scenario, result, HostRequirements{
		Libvirt: true,
		OVMF:    true,
		KVM:     runner.options().KVM,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	if err := CreateDisks(ctx, diskExec(nil), result.Disks); err != nil {
		t.Fatalf("create config apply data disk: %v", err)
	}
	katlctl := configApplyKatlctl(BuildKatlctl(t, ctx, repoRoot(t)), result)
	vm := runtime.VM
	vm.KVM = runner.options().KVM
	vm.RAMMiB = 2048
	vm.CPUs = 2
	vm.Timeout = 35 * time.Minute
	vm.VSock.Enabled = true
	vm.Agent.RequireHealth = true
	vm.Agent.Timeout = 30 * time.Second
	vm.PreserveNVRAM = true
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
		Name: "cp-1",
		Runtime: InstalledRuntimeConfig{
			Disk:            runtime.Disk,
			DiskFormat:      runtime.DiskFormat,
			ESPArtifacts:    runtime.ESPArtifacts,
			FixtureManifest: runtime.FixtureManifest,
			NodeMetadata:    runtime.NodeMetadata,
			VM:              vm,
		},
	}, VMRunner{})
	if err != nil {
		t.Fatalf("StartInstalledRuntimeNode() error = %v", err)
	}
	defer func() {
		if t.Failed() {
			if err := node.StopFailure("installed runtime config apply smoke failed"); err != nil && err != context.Canceled {
				t.Logf("StopFailure() error = %v", err)
			}
			return
		}
		if err := node.Stop(); err != nil && err != context.Canceled {
			t.Logf("Stop() error = %v", err)
		}
	}()
	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatalf("DialAgent() error = %v", err)
	}
	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()
	guest := NewGuestControl(node.Result, client)
	defer func() {
		if t.Failed() {
			guest.CollectDiagnostics(ctx, configApplyFailureDiagnostics())
		}
	}()
	if err := RunKatlcSmoke(ctx, guest); err != nil {
		t.Fatalf("katlc runtime smoke: %v", err)
	}
	assertInstalledSSHReady(t, ctx, guest)
	networkdConfig := guestCommandOutput(
		t, ctx, guest, "networkd-manager-policy",
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
		"/usr/bin/systemd-analyze", "cat-config", "systemd/networkd.conf",
	)
	if !containsAll(
		networkdConfig,
		"ManageForeignRoutes=no",
		"ManageForeignRoutingPolicyRules=no",
		"ManageForeignNextHops=no",
	) {
		t.Fatalf("merged systemd-networkd configuration is missing Kubernetes route policy:\n%s", networkdConfig)
	}
	guestCommand(t, ctx, guest, "networkd-active", "systemctl", "is-active", "systemd-networkd.service")
	endpoint := katlcEndpoint(t, node, plannedAddress)
	assertDefaultNetworkdCNIOwnership(t, ctx, guest)
	assertUnauthenticatedManagementRejected(t, ctx, endpoint)
	currentGeneration := currentGenerationFromGuest(t, ctx, guest)
	waitGenerationPromotion(t, ctx, guest, currentGeneration)
	enrollConfigApplyNode(t, ctx, result, katlctl, endpoint)
	guest, client = runConfigApplyModeSmoke(t, ctx, &node, guest, client, result, katlctl, endpoint, currentGeneration)
	node.Result.finish(StatusPassed, "", runner.time())
	if err := runner.Write(scenario, node.Result); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if worldScenario != nil {
		if err := worldScenario.WriteResult(WorldStatusPassed, ""); err != nil {
			t.Fatalf("write world scenario result: %v", err)
		}
	}
}

type networkdLinkStatus struct {
	Name                string `json:"Name"`
	Kind                string `json:"Kind"`
	AdministrativeState string `json:"AdministrativeState"`
	IPv4AddressState    string `json:"IPv4AddressState"`
	NetworkFile         string `json:"NetworkFile"`
	Addresses           []struct {
		Family       int    `json:"Family"`
		ConfigSource string `json:"ConfigSource"`
	} `json:"Addresses"`
	Routes []struct {
		ConfigSource string `json:"ConfigSource"`
	} `json:"Routes"`
}

func assertDefaultNetworkdCNIOwnership(t *testing.T, ctx context.Context, guest *GuestControl) {
	t.Helper()
	networkConfig := guestCommandOutput(
		t, ctx, guest, "networkd-default-policy",
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
		"/usr/bin/systemd-analyze", "cat-config", "systemd/network/80-katl-vmtest-dhcp.network",
	)
	if !containsAll(networkConfig, "[Match]\nType=ether\nKind=!*\n", "[Link]\nRequiredForOnline=routable\n") {
		t.Fatalf("default networkd policy does not isolate a routable physical management path:\n%s", networkConfig)
	}

	hostLink := defaultRouteLink(t, ctx, guest)
	// Boot completion does not order networkd's host link configuration: the
	// link can still be configuring its IPv6 side after the DHCPv4 lease binds.
	before := waitNetworkdStatuses(t, ctx, guest, "networkd-host-before-cni", []string{hostLink})[hostLink]
	assertNetworkdDHCPHost(t, before)

	for _, module := range []string{"veth", "dummy", "geneve", "vxlan", "bridge"} {
		guestCommand(t, ctx, guest, "networkd-cni-modprobe-"+module, "modprobe", module)
	}
	for _, command := range []struct {
		name string
		argv []string
	}{
		{name: "veth", argv: []string{"ip", "link", "add", "katl-veth0", "type", "veth", "peer", "name", "katl-veth1"}},
		{name: "cilium-host", argv: []string{"ip", "link", "add", "cilium_host", "type", "dummy"}},
		{name: "geneve", argv: []string{"ip", "link", "add", "katl-geneve", "type", "geneve", "id", "100", "remote", "192.0.2.1", "dstport", "6081"}},
		{name: "vxlan", argv: []string{"ip", "link", "add", "katl-vxlan", "type", "vxlan", "id", "100", "dev", hostLink, "dstport", "4789"}},
		{name: "bridge", argv: []string{"ip", "link", "add", "cni0", "type", "bridge"}},
	} {
		guestCommand(t, ctx, guest, "networkd-cni-create-"+command.name, command.argv...)
	}
	defer func() {
		for _, name := range []string{"katl-veth0", "cilium_host", "katl-geneve", "katl-vxlan", "cni0"} {
			_, _ = guest.RunCommand(ctx, GuestCommandRequest{
				Name:         "networkd-cni-delete-" + name,
				Argv:         []string{"ip", "link", "delete", name},
				AllowFailure: true,
			})
		}
	}()
	for _, name := range []string{"katl-veth0", "katl-veth1", "cilium_host", "katl-geneve", "katl-vxlan", "cni0"} {
		guestCommand(t, ctx, guest, "networkd-cni-up-"+name, "ip", "link", "set", name, "up")
	}
	guestCommand(t, ctx, guest, "networkd-cni-udev-settle", "udevadm", "settle")

	wantKinds := map[string]string{
		"katl-veth0":  "veth",
		"cilium_host": "dummy",
		"katl-geneve": "geneve",
		"katl-vxlan":  "vxlan",
		"cni0":        "bridge",
	}
	links := make([]string, 0, len(wantKinds))
	for name := range wantKinds {
		links = append(links, name)
	}
	slices.Sort(links)
	statuses := waitNetworkdStatuses(t, ctx, guest, "networkd-cni-status", links)
	for _, name := range links {
		status := statuses[name]
		if status.Kind != wantKinds[name] || status.AdministrativeState != "unmanaged" || status.NetworkFile != "" || status.IPv4AddressState != "off" {
			t.Errorf("networkd status for %s = %+v, want kind %q and unmanaged with IPv4 off", name, status, wantKinds[name])
		}
		for _, address := range status.Addresses {
			if address.Family == 2 || strings.HasPrefix(address.ConfigSource, "DHCP") {
				t.Errorf("networkd status for %s has unexpected address %+v", name, address)
			}
		}
		if len(status.Routes) != 0 {
			t.Errorf("networkd status for %s has unexpected routes %+v", name, status.Routes)
		}
	}
	after := networkdStatus(t, ctx, guest, "networkd-host-after-cni", hostLink)
	assertNetworkdDHCPHost(t, after)
}

func assertUnauthenticatedManagementRejected(t *testing.T, ctx context.Context, endpoint string) {
	t.Helper()
	for _, test := range []struct {
		name   string
		option grpc.DialOption
	}{
		{name: "plaintext", option: grpc.WithTransportCredentials(insecure.NewCredentials())},
		{name: "no-client-certificate", option: grpc.WithTransportCredentials(credentials.NewTLS(managementTLSWithoutClientCertificate(t)))},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, err := grpc.DialContext(ctx, endpoint, test.option)
			if err != nil {
				return
			}
			defer conn.Close()
			requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if response, err := agentapi.NewKatlcAgentClient(conn).GetNodeStatus(requestCtx, &agentapi.GetNodeStatusRequest{}); err == nil {
				t.Fatalf("unauthenticated management query succeeded: %+v", response)
			}
		})
	}
}

func managementTLSWithoutClientCertificate(t *testing.T) *tls.Config {
	t.Helper()
	identity, err := VMTestManagementIdentity(VMTestManagementClusterName)
	if err != nil {
		t.Fatal(err)
	}
	client, err := managementidentity.Client(identity, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	config, err := transport.ClientTLSConfig(client, "cp-1")
	if err != nil {
		t.Fatal(err)
	}
	config.Certificates = nil
	return config
}

func defaultRouteLink(t *testing.T, ctx context.Context, guest *GuestControl) string {
	t.Helper()
	output := guestCommandOutput(t, ctx, guest, "networkd-default-route", "ip", "-j", "route", "show", "default")
	var routes []struct {
		Dev string `json:"dev"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil || len(routes) == 0 || routes[0].Dev == "" {
		t.Fatalf("decode default route %q: routes=%+v err=%v", output, routes, err)
	}
	return routes[0].Dev
}

// waitNetworkdStatuses waits until networkd has finished setting up each link,
// whatever state the link settles in.
func waitNetworkdStatuses(t *testing.T, ctx context.Context, guest *GuestControl, commandName string, links []string) map[string]networkdLinkStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		argv := append([]string{"networkctl", "status"}, links...)
		argv = append(argv, "--json=short")
		record, err := guest.RunCommand(ctx, GuestCommandRequest{
			Name:         commandName,
			Argv:         argv,
			AllowFailure: true,
		})
		if err == nil {
			statuses, decodeErr := decodeNetworkdStatuses(readFile(t, record.Stdout))
			if decodeErr == nil && len(statuses) == len(links) {
				allSettled := true
				for _, link := range links {
					switch statuses[link].AdministrativeState {
					case "", "pending", "initialized", "configuring":
						allSettled = false
					}
				}
				if allSettled {
					return statuses
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("networkd did not report settled status for %v", links)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func decodeNetworkdStatuses(output string) (map[string]networkdLinkStatus, error) {
	statuses := map[string]networkdLinkStatus{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var status networkdLinkStatus
		if err := json.Unmarshal([]byte(line), &status); err != nil {
			return nil, err
		}
		statuses[status.Name] = status
	}
	return statuses, nil
}

func networkdStatus(t *testing.T, ctx context.Context, guest *GuestControl, commandName, link string) networkdLinkStatus {
	t.Helper()
	output := guestCommandOutput(t, ctx, guest, commandName, "networkctl", "status", link, "--json=short")
	statuses, err := decodeNetworkdStatuses(output)
	if err != nil {
		t.Fatalf("decode networkd status for %s: %v\n%s", link, err, output)
	}
	status, ok := statuses[link]
	if !ok {
		t.Fatalf("networkd status for %s missing from %q", link, output)
	}
	return status
}

func assertNetworkdDHCPHost(t *testing.T, status networkdLinkStatus) {
	t.Helper()
	if status.AdministrativeState != "configured" || status.NetworkFile == "" || status.IPv4AddressState != "routable" {
		t.Fatalf("host networkd status = %+v, want configured DHCP host link", status)
	}
	for _, address := range status.Addresses {
		if address.Family == 2 && address.ConfigSource == "DHCPv4" {
			return
		}
	}
	t.Fatalf("host networkd status = %+v, want DHCPv4 address", status)
}

func requirePlannedVMHost(t testTB, runner Runner, scenario Scenario, result Result, requirements HostRequirements) Result {
	t.Helper()
	result.start(runner.time())
	if err := runner.CheckHost(requirements); err != nil {
		status := StatusFailed
		if runner.options().Missing == MissingSkips {
			status = StatusSkipped
		}
		var prereq PrereqError
		if errors.As(err, &prereq) {
			result.Missing = prereq.Missing
		}
		result.finish(status, err.Error(), runner.time())
		if writeErr := runner.Write(scenario, result); writeErr != nil {
			t.Fatalf("write vmtest result for %q failed: %v\nvmtest run dir: %s", scenario.Name, writeErr, result.RunDir)
			return result
		}
		if status == StatusSkipped {
			t.Skipf("%v\nvmtest run dir: %s", err, result.RunDir)
			return result
		}
		t.Fatalf("%v\nvmtest run dir: %s", err, result.RunDir)
	}
	return result
}

func TestRequirePlannedVMHostWritesFailedResult(t *testing.T) {
	tb := &fakeTB{}
	runner := Runner{
		Options: Options{
			Enabled:   true,
			StateRoot: t.TempDir(),
			RunID:     "run-1",
			Missing:   MissingFails,
		},
		probe: probe{
			lookPath: func(string) (string, error) {
				return "", errors.New("missing VM command")
			},
		},
		now: func() time.Time {
			return time.Unix(10, 0)
		},
	}
	scenario := Scenario{Name: "installed-runtime-config-apply-modes"}
	result, err := runner.Plan(scenario)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}

	got := requirePlannedVMHost(tb, runner, scenario, result, HostRequirements{Libvirt: true})
	if !tb.failed || tb.skipped {
		t.Fatalf("failed=%v skipped=%v message=%q", tb.failed, tb.skipped, tb.message)
	}
	if got.Status != StatusFailed || !strings.Contains(got.FailureSummary, "virsh") {
		t.Fatalf("result = %#v", got)
	}
	data, err := os.ReadFile(got.Artifacts.Result)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var stored Result
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if stored.Status != StatusFailed || len(stored.Missing) == 0 || stored.Phases[0].Name != "host-prerequisites" {
		t.Fatalf("stored result = %#v", stored)
	}
}

// katlctlRunner is a katlctl binary together with the workstation config that
// every command it runs uses.
type katlctlRunner struct {
	bin    Katlctl
	config string
}

func configApplyKatlctl(bin Katlctl, result Result) katlctlRunner {
	return katlctlRunner{bin: bin, config: filepath.Join(result.RunDir, "katlctl", "enrollment", "katlctl.yaml")}
}

func katlcEndpoint(t *testing.T, node RunningInstalledRuntimeNode, plannedAddress string) string {
	t.Helper()
	address := first(node.Result.IPAddress, plannedAddress)
	if strings.TrimSpace(address) == "" {
		t.Fatalf("installed runtime node %q has no libvirt lease IP address", node.Name)
	}
	return net.JoinHostPort(address, "9443")
}

func enrollConfigApplyNode(t *testing.T, ctx context.Context, result Result, katlctl katlctlRunner, endpoint string, sshAuthorizedKey ...string) {
	t.Helper()
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatalf("split config apply management endpoint %q: %v", endpoint, err)
	}
	directory := filepath.Dir(katlctl.config)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatalf("create config apply enrollment directory: %v", err)
	}
	configPath := filepath.Join(directory, "cluster.yaml")
	key := first(sshAuthorizedKey...)
	if key == "" {
		key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDAxMjM0NTY3ODlhYmNkZWYwMTIzNDU2Nzg5YWJjZGVm katl@example"
	}
	source := `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: katl-smoke
spec:
  managementAuthentication: mtls
  controlPlaneEndpoint:
    host: ` + host + `
    port: 6443
  kubernetes:
    version: v1.36.1
  defaults:
    access:
      ssh:
        authorizedKeys:
          - ` + key + `
  nodes:
    - name: cp-1
      controlPlane: true
      management:
        address: ` + host + `
      install:
        systemDisk:
          byID: /dev/disk/by-id/virtio-katl-root
`
	if err := os.WriteFile(configPath, []byte(source), 0o600); err != nil {
		t.Fatalf("write config apply enrollment ClusterConfig: %v", err)
	}
	identity, err := VMTestManagementIdentity(VMTestManagementClusterName)
	if err != nil {
		t.Fatal(err)
	}
	if err := managementidentity.Write(filepath.Join(directory, "management", VMTestManagementClusterName+".katlkey"), identity); err != nil {
		t.Fatal(err)
	}
	runKatlctl(t, ctx, result, katlctl, "context-save", "context", "save", "--config", configPath)
}

func runConfigApplyModeSmoke(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, guest *GuestControl, client *AgentClient, result Result, katlctl katlctlRunner, endpoint, currentGeneration string) (*GuestControl, *AgentClient) {
	t.Helper()
	beforeLiveBootID := guestBootID(t, ctx, client)
	beforeSysext := readlinkOptional(t, ctx, guest, "/run/extensions/katl-kubernetes.raw")
	beforeBootSelection := readGuestFile(t, ctx, guest, "/var/lib/katl/boot/selection.json")
	rejectedGeneration := "2026.06.06-vmtest-rejected"
	liveGeneration := "2026.06.06-vmtest-live"
	stagedGeneration := "2026.06.06-vmtest-networkd"

	rejectedArgs := []string{
		"node", "apply", "validate",
		"--node", "cp-1",
	}
	rejectedArgs = append(rejectedArgs, configApplyConfigArgs(configApplyFixture(t, "rejected-live-without-preflight.yaml"))...)
	rejectedArgs = append(
		rejectedArgs,
		"--mode", "live",
		"--candidate-generation", rejectedGeneration,
		"--client-request-id", "vmtest-config-apply-rejected",
		"--actor", "installed-runtime config apply vmtest",
		"--output", "json",
	)
	rejectedOutput := runKatlctl(t, ctx, result, katlctl, "config-apply-validate-rejected", rejectedArgs...)
	var rejected agentapi.ConfigValidationResult
	mustUnmarshalProtoJSON(t, rejectedOutput, &rejected)
	if rejected.Accepted || !strings.Contains(strings.Join(rejected.Diagnostics, "\n"), "kernel module configuration is next-boot-only") {
		t.Fatalf("rejected validation = %+v, want fail closed next-boot module diagnostic", &rejected)
	}
	assertGuestMissing(t, ctx, guest, "/var/lib/katl/generations/"+rejectedGeneration)
	assertOptionalReadlink(t, ctx, guest, "/run/extensions/katl-kubernetes.raw", beforeSysext)
	if got := currentGenerationFromGuest(t, ctx, guest); got != currentGeneration {
		t.Fatalf("current generation after rejected validation = %q, want %q", got, currentGeneration)
	}

	rejectedApplyGeneration := "2026.06.06-vmtest-rejected-apply"
	rejectedAccepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-rejected", "live", rejectedApplyGeneration, configApplyFixture(t, "rejected-live-without-preflight.yaml"), true)
	rejectedStatus := waitKatlcOperationTerminal(t, ctx, endpoint, rejectedAccepted.OperationId)
	if rejectedStatus.Result != "failed" || rejectedStatus.GetRecoveryRequired() ||
		rejectedStatus.GetExternalMutationStarted() ||
		len(rejectedStatus.GetMutationScopes()) != 0 ||
		!strings.Contains(rejectedStatus.GetFailureReason(), "rejected") {
		t.Fatalf("rejected apply operation status = %+v, want fail closed before mutation", rejectedStatus)
	}
	assertGuestMissing(t, ctx, guest, "/var/lib/katl/generations/"+rejectedApplyGeneration)
	assertOptionalReadlink(t, ctx, guest, "/run/extensions/katl-kubernetes.raw", beforeSysext)
	if got := currentGenerationFromGuest(t, ctx, guest); got != currentGeneration {
		t.Fatalf("current generation after rejected apply = %q, want %q", got, currentGeneration)
	}
	assertGuestFileContains(t, ctx, guest, rejectedAccepted.RecordPath, `"operationKind": "generation-apply"`, `"result": "failed"`, "kernel module configuration is next-boot-only")

	guestCommand(t, ctx, guest, "start-service-before-mask", "systemctl", "start", "systemd-userdbd.service")
	guestCommand(t, ctx, guest, "service-active-before-mask", "systemctl", "is-active", "--quiet", "systemd-userdbd.service")
	guestCommand(t, ctx, guest, "bluetooth-load-before-policy", "modprobe", "bluetooth")
	assertGuestExists(t, ctx, guest, "/sys/module/bluetooth")
	liveAccepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-live", "", liveGeneration, configApplyFixture(t, "live-udev.yaml"), false)
	liveStatus := waitKatlcOperationTerminal(t, ctx, endpoint, liveAccepted.OperationId)
	if liveStatus.Result != operation.ResultSucceeded || liveStatus.ConfigApplyPhase != "active" || liveStatus.BootHealthPending {
		t.Fatalf("live operation status = %+v, want succeeded active config apply", liveStatus)
	}
	liveGenerationStatus := katlctlGenerationStatus(t, ctx, result, katlctl, endpoint, "status-live", liveGeneration)
	if liveGenerationStatus.GetCommitState() != "committed" || liveGenerationStatus.GetHealthState() != "healthy" {
		t.Fatalf("live generation is not ready for dependent operations: %+v", liveGenerationStatus)
	}
	if got := guestBootID(t, ctx, client); got != beforeLiveBootID {
		t.Fatalf("live apply rebooted the node: boot ID %s became %s", beforeLiveBootID, got)
	}
	assertMaskedUnits(t, ctx, guest, "live")
	// vm.swappiness is a first-time assignment of an existing kernel parameter.
	if got := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "live-swappiness", "sysctl", "-n", "vm.swappiness")); got != "20" {
		t.Fatalf("live vm.swappiness = %q, want 20", got)
	}

	if liveGenerationStatus.GetConfigApply().GetPhase() != "active" || liveGenerationStatus.GetConfigApply().GetRequestedApplyMode() != "auto" || liveGenerationStatus.GetConfigApply().GetAcceptedApplyMode() != "live" {
		t.Fatalf("live katlctl generation status = %+v, want active auto->live config apply", liveGenerationStatus.GetConfigApply())
	}
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+liveGeneration+"/spec.json", `"generationID": "`+liveGeneration+`"`, `"previousGenerationID": "`+currentGeneration+`"`)
	if beforeSysext != "" {
		assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+liveGeneration+"/spec.json", `"name": "kubernetes"`)
	}
	assertGuestFileContains(
		t, ctx, guest, "/var/lib/katl/generations/"+liveGeneration+"/status.json",
		`"commitState": "committed"`,
		`"bootState": "good"`,
		`"healthState": "healthy"`,
		`"committedByOperationID": "`+liveAccepted.OperationId+`"`,
	)
	assertGuestFileContains(
		t, ctx, guest, "/var/lib/katl/generations/"+liveGeneration+"/config-apply-status.json",
		`"phase": "active"`,
		`"requestedApplyMode": "auto"`,
		`"acceptedApplyMode": "live"`,
		`"domain": "host-configuration"`,
		`"sets": [`,
		`"vmtest-udev"`,
		`"target": "sysctl vm.swappiness"`,
		`existing devices will not be retriggered`,
	)
	guestCommand(
		t, ctx, guest, "effective-udev-rule",
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
		"/usr/bin/test", "-r", "/etc/udev/rules.d/80-katl-vmtest.rules",
	)
	assertGuestFileContains(t, ctx, guest, liveAccepted.RecordPath, `"operationKind": "generation-apply"`, `"applyMode": "auto"`, `"configApplyPhase": "active"`)
	assertGuestFileContains(
		t, ctx, guest, "/var/lib/katl/boot/selection.json",
		`"defaultGenerationID": "`+liveGeneration+`"`,
		`"activeGenerationID": "`+liveGeneration+`"`,
		`"bootedGenerationID": "`+currentGeneration+`"`,
		`"pendingHealthValidation": false`,
	)
	networkAccepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-network-live", "live", "2026.06.06-vmtest-network-live", configApplyFixture(t, "live-network.yaml"), false)
	networkStatus := waitKatlcOperationTerminal(t, ctx, endpoint, networkAccepted.OperationId)
	if networkStatus.Result != operation.ResultSucceeded || networkStatus.ConfigApplyPhase != "active" || networkStatus.BootHealthPending {
		t.Fatalf("live network status = %+v", networkStatus)
	}
	if got := guestBootID(t, ctx, client); got != beforeLiveBootID {
		t.Fatalf("network apply rebooted the node: %s became %s", beforeLiveBootID, got)
	}
	assertGuestAddress(t, ctx, guest, "198.51.100.76", 32)
	guestCommand(t, ctx, guest, "live-netdev-initialized", "udevadm", "wait", "--timeout=30", "--initialized=yes", "/sys/class/net/katllive0")
	if output := guestCommandOutput(t, ctx, guest, "live-netdev", "ip", "-d", "link", "show", "katllive0"); !strings.Contains(output, "alias katl-live-netdev") {
		t.Fatalf("live netdev did not use its .link file: %s", output)
	}
	runNetdevLifecycleSmoke(t, ctx, guest, result, katlctl, endpoint)

	// The systemd smoke replaces the network sets, so Katl deletes katllive0.
	runSystemdUnitSmoke(t, ctx, guest, client, result, katlctl, endpoint)
	if removed, err := guest.RunCommand(ctx, GuestCommandRequest{Name: "removed-netdev", Argv: []string{"ip", "link", "show", "katllive0"}, AllowFailure: true}); err != nil || removed.ExitStatus == 0 {
		t.Fatalf("removed netdev katllive0 is still present: result=%+v err=%v", removed, err)
	}
	activeGeneration := runVolumeRemovalSmoke(t, ctx, guest, result, katlctl, endpoint)

	assertGuestNonLoopbackLink(t, ctx, guest)
	if cmdline := readGuestFile(t, ctx, guest, "/proc/cmdline"); strings.Contains(cmdline, "katl.vmtest.config_apply_kernel=1") {
		t.Fatalf("kernel command-line change became active before reboot: %s", cmdline)
	}
	stagedAccepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-staged-networkd", "next-boot", stagedGeneration, configApplyFixture(t, "next-boot-networkd.yaml"), false)
	stagedStatus := waitKatlcOperationTerminal(t, ctx, endpoint, stagedAccepted.OperationId)
	if stagedStatus.Result != operation.ResultSucceeded || stagedStatus.ConfigApplyPhase != "next-boot" {
		t.Fatalf("staged operation status = %+v, want succeeded next-boot config apply", stagedStatus)
	}
	stagedGenerationStatus := katlctlGenerationStatus(t, ctx, result, katlctl, endpoint, "status-staged", stagedGeneration)
	if stagedGenerationStatus.GetConfigApply().GetPhase() != "next-boot" || stagedGenerationStatus.GetConfigApply().GetAcceptedApplyMode() != "next-boot" {
		t.Fatalf("staged katlctl generation status = %+v, want next-boot config apply", stagedGenerationStatus.GetConfigApply())
	}
	assertGuestFileContains(
		t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/spec.json",
		`"generationID": "`+stagedGeneration+`"`,
		`"previousGenerationID": "`+activeGeneration+`"`,
		`"configuredKernelCommandLine": [`,
		`"katl.vmtest.config_apply_kernel=1"`,
	)
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/status.json", `"commitState": "committed"`, `"bootState": "trying"`, `"committedByOperationID": "`+stagedAccepted.OperationId+`"`)
	assertGuestFileContains(
		t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/config-apply-status.json",
		`"phase": "next-boot"`,
		`"acceptedApplyMode": "next-boot"`,
		`"domain": "host-configuration"`,
		`"domain": "kernel-command-line"`,
		`"target": "sysfs /sys/module/printk/parameters/tim*"`,
		`"target": "containerd configuration /etc/containerd/conf.d/80-katl-vmtest.toml"`,
	)
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/confext/etc/systemd/network/80-katl-vmtest-dhcp.network.d/50-address.conf", "Address=198.51.100.77/32")
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/confext/etc/containerd/conf.d/80-katl-vmtest.toml", "oom_score = 123")
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/boot/selection.json", `"defaultGenerationID": "`+activeGeneration+`"`, `"targetBootGenerationID": "`+stagedGeneration+`"`, `"trialGenerationID": "`+stagedGeneration+`"`, `"previousKnownGoodGenerationID": "`+activeGeneration+`"`, `"pendingTransactionID": "`+stagedAccepted.OperationId+`"`, `"pendingHealthValidation": true`)
	retained := katlctlGenerationStatus(t, ctx, result, katlctl, endpoint, "status-retained-generation", currentGeneration)
	if retained.GetGenerationId() != currentGeneration {
		t.Fatalf("retained generation = %q, want %q", retained.GetGenerationId(), currentGeneration)
	}
	assertOptionalReadlink(t, ctx, guest, "/run/extensions/katl-kubernetes.raw", beforeSysext)
	assertGuestFileContains(t, ctx, guest, stagedAccepted.RecordPath, `"operationKind": "generation-stage"`, `"configApplyPhase": "next-boot"`)
	if afterBootSelection := readGuestFile(t, ctx, guest, "/var/lib/katl/boot/selection.json"); afterBootSelection == beforeBootSelection {
		t.Fatalf("boot selection did not change after staged config apply")
	}

	previousBootID := guestBootID(t, ctx, client)
	// Installation history is diagnostic; it cannot veto a valid runtime boot.
	writeGuestFile(t, ctx, guest, "/var/lib/katl/install/status.json", []byte("{damaged installation history"), 0o600)
	runKatlctl(
		t, ctx, result, katlctl, "host-reboot-staged-generation",
		"node", "reboot", "cp-1",
		"--timeout", "3m",
	)
	_ = client.Close()
	guest, client = reconnectGuestAfterBoot(t, ctx, node, previousBootID)
	if got := currentGenerationFromGuest(t, ctx, guest); got != stagedGeneration {
		t.Fatalf("booted generation = %q, want staged networkd generation %q", got, stagedGeneration)
	}
	assertInstalledSSHReady(t, ctx, guest)
	if err := guest.WaitBootComplete(ctx); err != nil {
		t.Fatal(err)
	}
	waitGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/status.json", `"commitState": "committed"`, `"bootState": "good"`, `"healthState": "healthy"`)
	guestCommand(
		t, ctx, guest, "effective-networkd-config",
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
		"/usr/bin/test", "-r", "/etc/systemd/network/80-katl-vmtest-dhcp.network.d/50-address.conf",
	)
	assertGuestAddress(t, ctx, guest, "198.51.100.77", 32)
	guestCommand(t, ctx, guest, "booted-netdev-initialized", "udevadm", "wait", "--timeout=30", "--initialized=yes", "/sys/class/net/katllive0")
	if output := guestCommandOutput(t, ctx, guest, "booted-netdev", "ip", "-d", "link", "show", "katllive0"); !strings.Contains(output, "alias katl-live-netdev") {
		t.Fatalf("booted netdev did not use its .link file: %s", output)
	}
	assertGuestFileContains(t, ctx, guest, "/proc/cmdline", "katl.vmtest.config_apply_kernel=1")
	assertMaskedUnits(t, ctx, guest, "reboot")
	if got := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "reboot-swappiness", "sysctl", "-n", "vm.swappiness")); got != "20" {
		t.Fatalf("vm.swappiness after reboot = %q, want 20", got)
	}
	assertGuestFileContains(t, ctx, guest, "/proc/cmdline", "module_blacklist=bluetooth,btusb,btmtk", "modprobe.blacklist=bluetooth,btusb,btmtk")
	blocked, err := guest.RunCommand(ctx, GuestCommandRequest{Name: "bluetooth-load-blocked", Argv: []string{"modprobe", "bluetooth"}, AllowFailure: true})
	if err != nil || blocked.ExitStatus == 0 {
		t.Fatalf("Bluetooth module load should be refused: result=%+v err=%v", blocked, err)
	}
	assertGuestMissing(t, ctx, guest, "/sys/module/bluetooth")
	assertGuestMissing(t, ctx, guest, "/sys/module/btusb")
	if got := strings.TrimSpace(guestCommandOutput(
		t, ctx, guest, "effective-sysfs",
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
		"/usr/bin/cat", "/sys/module/printk/parameters/time",
	)); got != "N" {
		t.Fatalf("printk time sysfs value = %q, want N", got)
	}
	guestCommand(t, ctx, guest, "start-containerd-with-overlay", "systemctl", "start", "containerd.service")
	guestCommand(t, ctx, guest, "containerd-active-after-overlay", "systemctl", "is-active", "--quiet", "containerd.service")
	containerdPID := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "containerd-main-pid", "systemctl", "show", "--property=MainPID", "--value", "containerd.service"))
	if containerdPID == "" || containerdPID == "0" {
		t.Fatalf("containerd MainPID = %q, want running process", containerdPID)
	}
	if got := strings.TrimSpace(guestCommandOutput(
		t, ctx, guest, "containerd-oom-score",
		"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
		"/usr/bin/cat", "/proc/"+containerdPID+"/oom_score_adj",
	)); got != "123" {
		t.Fatalf("containerd oom_score_adj = %q, want imported overlay value 123", got)
	}
	if cmdline := readGuestFile(t, ctx, guest, "/proc/cmdline"); strings.Contains(cmdline, "katl.vmtest.initial_kernel=1") {
		t.Fatalf("replaced initial kernel argument remained active after reboot: %s", cmdline)
	}
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/boot/selection.json", `"defaultGenerationID": "`+stagedGeneration+`"`, `"bootedGenerationID": "`+stagedGeneration+`"`, `"pendingHealthValidation": false`)
	assertGuestFileContains(t, ctx, guest, "/var/lib/katl/generations/"+stagedGeneration+"/config-apply-status.json", `"phase": "active"`, `"acceptedApplyMode": "next-boot"`)
	bootedGenerationStatus := katlctlGenerationStatus(t, ctx, result, katlctl, endpoint, "status-booted-networkd", stagedGeneration)
	if bootedGenerationStatus.GetConfigApply().GetPhase() != "active" || bootedGenerationStatus.GetConfigApply().GetAcceptedApplyMode() != "next-boot" {
		t.Fatalf("booted networkd katlctl generation status = %+v, want active next-boot config apply", bootedGenerationStatus.GetConfigApply())
	}
	guest, client = runOptionalUnitFailureSmoke(t, ctx, node, guest, client, result, katlctl, endpoint)
	optionalGeneration := currentGenerationFromGuest(t, ctx, guest)
	guest, client = runOrdinaryBootFailureSmoke(t, ctx, node, guest, client, result, katlctl, optionalGeneration)
	guest, client = runManagementNetworkFailureSmoke(t, ctx, node, guest, client, result, katlctl, endpoint, optionalGeneration)
	guest, client = runFailedTrialRecoverySmoke(t, ctx, node, guest, client, result, katlctl, endpoint, optionalGeneration)
	guest, client = runExtensionBootHealthSmoke(t, ctx, node, guest, client, result, katlctl, endpoint, optionalGeneration, "vmtest-required", "27",
		map[string]string{"usr/lib/systemd/system/vmtest-required.service": "[Unit]\nDescription=Fail a unit required for boot health\n[Service]\nType=oneshot\nExecStart=/usr/bin/false\n[Install]\nWantedBy=multi-user.target\n"},
		"      units:\n        - name: vmtest-required.service\n          enable: true\n          requiredForBootHealth: true\n",
		"Failed to start katl-configured-units-check.service")
	// A stall before sysinit.target must time out so that networking,
	// management, and boot health still run and reject the trial.
	assertExtensionActivationProductionTimeout(t, ctx, guest, "before-stall")
	guest, client = runExtensionBootHealthSmoke(t, ctx, node, guest, client, result, katlctl, endpoint, optionalGeneration, "vmtest-stall", "28",
		map[string]string{"usr/lib/systemd/system/katl-system-extensions-activate.service.d/50-vmtest-stall.conf": extensionActivationStallDropIn}, "",
		"vmtest stall: TimeoutStartUSec=[5s]",
		"katl-system-extensions-activate.service: start-pre operation timed out",
		"katl-system-extensions-activate.service: Failed with result 'timeout'")
	assertExtensionActivationProductionTimeout(t, ctx, guest, "after-fallback")
	assertBootstrappedKubernetesSysextChangeRejected(t, ctx, guest, endpoint)
	shutdownGuestThroughKatlctl(t, ctx, result, katlctl, endpoint, node, client)
	return guest, nil
}

func runOptionalUnitFailureSmoke(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, guest *GuestControl, client *AgentClient, result Result, katlctl katlctlRunner, endpoint string) (*GuestControl, *AgentClient) {
	t.Helper()
	candidate := "2026.06.06-vmtest-optional-unit"
	accepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-optional-unit", "next-boot", candidate, configApplyFixture(t, "next-boot-optional-unit-failure.yaml"), false)
	status := waitKatlcOperationTerminal(t, ctx, endpoint, accepted.OperationId)
	if status.Result != operation.ResultSucceeded || status.ConfigApplyPhase != "next-boot" {
		t.Fatalf("optional-unit staging status = %+v, want succeeded next-boot apply", status)
	}

	previousBootID := guestBootID(t, ctx, client)
	runKatlctl(t, ctx, result, katlctl, "host-reboot-optional-unit", "node", "reboot", "cp-1", "--timeout", "3m")
	_ = client.Close()
	guest, client = reconnectGuestAfterGeneration(t, ctx, node, previousBootID, candidate)

	_, candidateStatus := generationRecordsFromGuest(t, ctx, guest, candidate)
	if !generation.IsKnownGood(candidateStatus) {
		t.Fatalf("optional-unit generation status = %#v, want good/healthy", candidateStatus)
	}
	for _, unit := range []string{"vmtest-optional-failure.service", "vmtest-optional-early-failure.service"} {
		if got := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "optional-unit-result-"+unit, "systemctl", "show", "--property=Result", "--value", unit)); got != "exit-code" {
			t.Fatalf("optional unit %s result = %q, want exit-code", unit, got)
		}
	}
	// A configured unit's native Before=network-pre.target holds at boot: it
	// starts while networking and management services are still waiting.
	if got := strings.Fields(guestCommandOutput(t, ctx, guest, "before-network-states", "systemd-run", "--quiet", "--wait", "--collect", "--pipe", "/usr/bin/cat", "/run/vmtest-before-network")); !slices.Equal(got, []string{"inactive", "inactive", "inactive"}) {
		t.Fatalf("networkd, sshd, and katlc-agent states when a pre-network unit started = %q, want inactive", got)
	}
	statusOutput := runKatlctl(t, ctx, result, katlctl, "status-after-optional-unit-failure", "node", "status", "cp-1")
	if !strings.Contains(string(statusOutput), candidate) {
		t.Fatalf("katlctl status does not show promoted generation %s:\n%s", candidate, statusOutput)
	}
	return guest, client
}

func runOrdinaryBootFailureSmoke(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, guest *GuestControl, client *AgentClient, result Result, katlctl katlctlRunner, generationID string) (*GuestControl, *AgentClient) {
	t.Helper()
	const (
		marker        = "/var/lib/katl/test-artifacts/fail-ordinary-boot"
		failedBootID  = "/var/lib/katl/test-artifacts/failed-ordinary-boot-id"
		requiredUnit  = "vmtest-ordinary-boot.service"
		defaultEFIVar = "/sys/firmware/efi/efivars/LoaderEntryDefault-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f"
	)

	// The preceding configuration trial passed with the marker absent.
	// Trigger the failure through its installed native systemd unit.
	beforeDefault := guestCommandOutput(t, ctx, guest, "efi-default-before-ordinary-boot", "systemd-run", "--quiet", "--wait", "--collect", "--pipe", "/usr/bin/od", "-An", "-v", "-tx1", defaultEFIVar)
	writeGuestFile(t, ctx, guest, marker, []byte("fail on boot\n"), 0o600)
	previousBootID := guestBootID(t, ctx, client)
	_, rebootErr := runKatlctlOutcome(t, ctx, result, katlctl, "reboot-into-ordinary-unit-failure", "node", "reboot", "cp-1", "--timeout", "3m")
	if rebootErr == nil || !containsAll(rebootErr.Error(), "failed boot health on generation "+generationID, requiredUnit) {
		t.Fatalf("ordinary reboot error = %v, want failed boot health on %s naming %s", rebootErr, generationID, requiredUnit)
	}
	_ = client.Close()
	guest, client = reconnectGuestAfterBoot(t, ctx, node, previousBootID)

	jsonStatus := runKatlctl(t, ctx, result, katlctl, "status-after-ordinary-unit-failure", "node", "status", "cp-1", "--output", "json")
	var status struct {
		Generation           string `json:"generation"`
		Health               string `json:"health"`
		BootHealthDiagnostic string `json:"bootHealthDiagnostic"`
	}
	if err := json.Unmarshal(jsonStatus, &status); err != nil {
		t.Fatal(err)
	}
	if status.Generation != generationID || status.Health != "failed" || !strings.Contains(status.BootHealthDiagnostic, requiredUnit) {
		t.Fatalf("unexpected node status after ordinary failure:\n%s", jsonStatus)
	}
	jsonGenerations := runKatlctl(t, ctx, result, katlctl, "generations-after-ordinary-unit-failure", "node", "generations", "list", "cp-1", "--output", "json")
	var generations agentapi.ListGenerationsResponse
	mustUnmarshalProtoJSON(t, jsonGenerations, &generations)
	if generations.GetDefaultGenerationId() != generationID || generations.GetNextBootGenerationId() != generationID {
		t.Fatalf("ordinary failure changed the boot selection:\n%s", jsonGenerations)
	}
	index := slices.IndexFunc(generations.GetGenerations(), func(gen *agentapi.Generation) bool { return gen.GetGenerationId() == generationID })
	if index < 0 || generations.Generations[index].GetBootState() != generation.BootStateGood || generations.Generations[index].GetHealthState() != generation.HealthStateHealthy {
		t.Fatalf("ordinary failure changed the generation's known-good status:\n%s", jsonGenerations)
	}
	afterDefault := guestCommandOutput(t, ctx, guest, "efi-default-after-ordinary-boot", "systemd-run", "--quiet", "--wait", "--collect", "--pipe", "/usr/bin/od", "-An", "-v", "-tx1", defaultEFIVar)
	if afterDefault != beforeDefault {
		t.Fatalf("ordinary failure changed EFI default bytes from %q to %q", beforeDefault, afterDefault)
	}
	journal := guestCommandOutput(t, ctx, guest, "ordinary-boot-recovery-journal", "journalctl", "-b", "--no-pager", "-u", "katl-boot-recovery.service")
	if !containsAll(journal, "running generation "+generationID+" stays known-good", "rebootRequested=false") {
		t.Fatalf("ordinary boot recovery journal lacks the kept-known-good outcome:\n%s", journal)
	}
	guestCommand(t, ctx, guest, "ordinary-boot-recovery-refused-reboot", "systemctl", "is-failed", "--quiet", "katl-boot-recovery.service")
	firstFailureID := strings.TrimSpace(readGuestFile(t, ctx, guest, failedBootID))
	if got := guestBootID(t, ctx, client); got != firstFailureID {
		t.Fatalf("boot ID changed after the unit failed from %s to %s", firstFailureID, got)
	}

	guestCommand(t, ctx, guest, "repair-ordinary-boot-unit", "systemd-run", "--quiet", "--wait", "--collect", "/usr/bin/rm", "--", marker)
	runKatlctl(t, ctx, result, katlctl, "reboot-after-ordinary-unit-repair", "node", "reboot", "cp-1", "--timeout", "3m")
	_ = client.Close()
	guest, client = reconnectGuestAfterBoot(t, ctx, node, firstFailureID)
	assertBootedGenerationIdentity(t, ctx, guest, generationFromGuest(t, ctx, guest, generationID))
	return guest, client
}

func runManagementNetworkFailureSmoke(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, guest *GuestControl, client *AgentClient, result Result, katlctl katlctlRunner, endpoint, fallbackGeneration string) (*GuestControl, *AgentClient) {
	t.Helper()
	candidate := "2026.06.06-vmtest-management-network"
	accepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-management-network", "next-boot", candidate, configApplyFixture(t, "next-boot-management-network-failure.yaml"), false)
	status := waitKatlcOperationTerminal(t, ctx, endpoint, accepted.OperationId)
	if status.Result != operation.ResultSucceeded || status.ConfigApplyPhase != "next-boot" {
		t.Fatalf("management-network staging status = %+v, want succeeded next-boot apply", status)
	}

	previousBootID := guestBootID(t, ctx, client)
	bootEventCursor := len(node.handle.notifyEvents())
	repo := repoRoot(t)
	rebootCtx, cancelReboot := context.WithCancel(ctx)
	candidateCtx, cancelCandidate := context.WithCancelCause(ctx)
	defer cancelCandidate(nil)
	rebootDone := make(chan struct{})
	var rebootErr error
	phaseStart := time.Now()
	go func() {
		defer close(rebootDone)
		_, rebootErr = recordKatlctlOutcome(rebootCtx, repo, result, katlctl, "host-reboot-management-network", "node", "reboot", "cp-1", "--timeout", "4m")
		cancelCandidate(fmt.Errorf("katlctl reboot completed before candidate discovery: %v", rebootErr))
	}()
	defer func() {
		cancelReboot()
		<-rebootDone
	}()
	_ = client.Close()
	candidateClient, candidateBootID, err := discoverGuestBoot(candidateCtx, node, previousBootID, bootEventCursor)
	if err != nil {
		t.Fatalf("discover management-network candidate after boot %s: %v", previousBootID, err)
	}
	defer candidateClient.Close()
	candidateGuest := NewGuestControl(node.Result, candidateClient)
	if err := RunKatlcSmoke(candidateCtx, candidateGuest); err != nil {
		t.Fatalf("katlc runtime smoke on candidate boot %s: %v", candidateBootID, err)
	}
	if booted := currentGenerationFromGuest(t, ctx, candidateGuest); booted != candidate {
		t.Fatalf("boot %s is running generation %s, want candidate %s", candidateBootID, booted, candidate)
	}

	invocation := waitForManagementNetworkExpiryArm(t, ctx, candidateGuest, candidateBootID, rebootDone)
	assertRequiredManagementLinkOffline(t, ctx, candidateGuest)
	select {
	case <-rebootDone:
		t.Fatalf("katlctl reboot completed before management network expiry: %v", rebootErr)
	default:
	}
	// Write to this invocation's armed FIFO. A stale or missing reader fails
	// immediately, without leaving a request for another invocation.
	guestCommand(t, ctx, candidateGuest, "expire-management-network",
		"dd", "if=/dev/zero", "of=/run/katl-vmtest-boot-health/"+invocation,
		"bs=1", "count=1", "oflag=nonblock", "conv=nocreat", "status=none")

	<-rebootDone
	if rebootErr == nil || !strings.Contains(rebootErr.Error(), "rejected generation "+candidate+" during boot health and returned on generation "+fallbackGeneration) {
		t.Fatalf("management-network reboot error = %v, want actionable fallback report", rebootErr)
	}
	guest, client = reconnectGuestAfterBoot(t, ctx, node, candidateBootID)
	if booted := currentGenerationFromGuest(t, ctx, guest); booted != fallbackGeneration {
		t.Fatalf("boot after candidate %s is running generation %s, want fallback %s", candidateBootID, booted, fallbackGeneration)
	}
	t.Logf("management-network failure phase: %s", time.Since(phaseStart))

	_, candidateStatus := generationRecordsFromGuest(t, ctx, guest, candidate)
	if candidateStatus.BootState != generation.BootStateFailed || candidateStatus.HealthState != generation.HealthStateUnhealthy {
		t.Fatalf("management-network candidate status = %#v, want failed/unhealthy", candidateStatus)
	}
	journalBoot := "--boot=" + strings.ReplaceAll(candidateBootID, "-", "")
	guestCommand(t, ctx, guest, "management-network-injected-expiry-journal", "journalctl", journalBoot,
		"--unit=katl-boot-health.service", "--no-pager", "--grep=^vmtest management network expiry injected$",
		"_SYSTEMD_INVOCATION_ID="+invocation)
	healthJournal := guestCommandOutput(t, ctx, guest, "management-network-health-journal", "journalctl", journalBoot, "--no-pager", "-u", "katl-boot-health.service")
	if !containsAll(healthJournal, "generation="+candidate+" result=failure", "management network unavailable: configured management network is not ready after 90 seconds: required links are not online: vmtest-mgmt") {
		t.Fatalf("boot-health journal for boot %s lacks the named-link deadline failure:\n%s", candidateBootID, healthJournal)
	}
	recoveryJournal := guestCommandOutput(t, ctx, guest, "management-network-recovery-journal", "journalctl", journalBoot, "--no-pager", "-u", "katl-boot-recovery.service")
	if !containsAll(recoveryJournal, "generation="+candidate+" result=failure default="+fallbackGeneration, "rebootRequested=true") {
		t.Fatalf("recovery journal for boot %s lacks automatic fallback:\n%s", candidateBootID, recoveryJournal)
	}
	t.Logf("management-network candidate boot %s recovered to fallback boot %s", candidateBootID, guestBootID(t, ctx, client))
	return guest, client
}

func discoverGuestBoot(ctx context.Context, node *RunningInstalledRuntimeNode, previousBootID string, cursor int) (*AgentClient, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		var err error
		cursor, err = node.handle.notify.waitForUnitActive(ctx, node.handle.done, "katl-vmtest-agent-ready.target", cursor)
		if err != nil {
			return nil, "", fmt.Errorf("wait for guest agent readiness: %w", err)
		}
		client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
		if err != nil {
			return nil, "", fmt.Errorf("connect to ready guest agent: %w", err)
		}
		health, err := client.Health(ctx)
		if err != nil {
			_ = client.Close()
			return nil, "", fmt.Errorf("read boot ID from ready guest agent: %w", err)
		}
		bootID := strings.TrimSpace(health.GetBootId())
		if bootID != "" && bootID != previousBootID {
			return client, bootID, nil
		}
		_ = client.Close()
		if bootID == "" {
			return nil, "", errors.New("ready guest agent returned an empty boot ID")
		}
	}
}

func waitForManagementNetworkExpiryArm(t *testing.T, ctx context.Context, guest *GuestControl, bootID string, rebootDone <-chan struct{}) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		record, err := guest.RunCommand(ctx, GuestCommandRequest{
			Name: "management-network-expiry-armed",
			Argv: []string{"journalctl", "--boot=" + strings.ReplaceAll(bootID, "-", ""), "--unit=katl-boot-health.service", "--output=json", "--lines=1", "--no-pager",
				"--grep=^vmtest management network expiry armed: required links are not online: vmtest-mgmt(,|$)"},
			AllowFailure: true, // journalctl exits 1 when no event has matched yet.
		})
		if err != nil {
			t.Fatalf("read expiry arm event for candidate boot %s: %v", bootID, err)
		}
		if record.ExitStatus == 0 {
			var entry struct {
				Invocation string `json:"_SYSTEMD_INVOCATION_ID"`
			}
			output := readFile(t, record.Stdout)
			if err := json.Unmarshal([]byte(output), &entry); err != nil {
				t.Fatalf("decode expiry arm event: %v\n%s", err, output)
			}
			if id, err := hex.DecodeString(entry.Invocation); err != nil || len(id) != 16 {
				t.Fatalf("expiry arm event has invalid invocation ID: %q", entry.Invocation)
			}
			return entry.Invocation
		}
		if stderr := readFile(t, record.Stderr); record.ExitStatus != 1 || strings.TrimSpace(stderr) != "" {
			t.Fatalf("read expiry arm event for candidate boot %s: journalctl exited %d\n%s", bootID, record.ExitStatus, stderr)
		}
		select {
		case <-rebootDone:
			t.Fatalf("katlctl reboot completed before boot-health armed expiry on candidate boot %s", bootID)
		case <-ctx.Done():
			t.Fatalf("boot-health did not arm expiry for vmtest-mgmt on candidate boot %s: %v", bootID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertRequiredManagementLinkOffline(t *testing.T, ctx context.Context, guest *GuestControl) {
	t.Helper()
	output := guestCommandOutput(t, ctx, guest, "management-network-link-state", "networkctl", "list", "--json=short", "--no-pager")
	var state struct {
		Interfaces []struct {
			Name              string
			NetworkFile       string
			RequiredForOnline bool
			OnlineState       string
			OperationalState  string
		}
	}
	if err := json.Unmarshal([]byte(output), &state); err != nil {
		t.Fatalf("decode networkd link state: %v\n%s", err, output)
	}
	for _, link := range state.Interfaces {
		if link.Name == "vmtest-mgmt" && link.NetworkFile != "" && link.RequiredForOnline && link.OnlineState == "offline" && link.OperationalState != "routable" {
			return
		}
	}
	t.Fatalf("networkd does not show vmtest-mgmt required and offline:\n%s", output)
}

func runFailedTrialRecoverySmoke(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, guest *GuestControl, client *AgentClient, result Result, katlctl katlctlRunner, endpoint, fallbackGeneration string) (*GuestControl, *AgentClient) {
	t.Helper()
	candidate := "2026.06.06-vmtest-failed-trial"
	accepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-failed-trial", "next-boot", candidate, configApplyFixture(t, "next-boot-required-unit-failure.yaml"), false)
	status := waitKatlcOperationTerminal(t, ctx, endpoint, accepted.OperationId)
	if status.Result != operation.ResultSucceeded || status.ConfigApplyPhase != "next-boot" {
		t.Fatalf("failed-trial staging status = %+v, want succeeded next-boot apply", status)
	}
	selection := bootSelectionFromGuest(t, ctx, guest)
	if selection.TrialGenerationID != candidate || selection.PreviousKnownGoodGenerationID != fallbackGeneration {
		t.Fatalf("failed-trial selection = %#v", selection)
	}

	previousBootID := guestBootID(t, ctx, client)
	_, rebootErr := runKatlctlOutcome(t, ctx, result, katlctl, "host-reboot-failed-trial", "node", "reboot", "cp-1", "--timeout", "3m")
	if rebootErr == nil || !strings.Contains(rebootErr.Error(), "rejected generation "+candidate+" during boot health and returned on generation "+fallbackGeneration) {
		t.Fatalf("failed-trial reboot error = %v, want actionable fallback report", rebootErr)
	}
	_ = client.Close()
	guest, client = reconnectGuestAfterGeneration(t, ctx, node, previousBootID, fallbackGeneration)

	fallbackSpec := generationFromGuest(t, ctx, guest, fallbackGeneration)
	assertBootedGenerationIdentity(t, ctx, guest, fallbackSpec)
	_, candidateStatus := generationRecordsFromGuest(t, ctx, guest, candidate)
	if candidateStatus.BootState != generation.BootStateFailed || candidateStatus.HealthState != generation.HealthStateUnhealthy {
		t.Fatalf("failed candidate status = %#v, want failed/unhealthy", candidateStatus)
	}
	selection = bootSelectionFromGuest(t, ctx, guest)
	if selection.DefaultGenerationID != fallbackGeneration || selection.BootedGenerationID != fallbackGeneration || selection.PendingHealthValidation || selection.RecoveryRequired {
		t.Fatalf("fallback boot selection = %#v", selection)
	}
	if got := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "boot-deadman-after-fallback", "systemctl", "show", "--property=ActiveState", "--value", "katl-boot-deadman.timer")); got != "active" {
		t.Fatalf("boot deadline timer = %q, want active with its trial switch cleared", got)
	}
	assertGuestMissing(t, ctx, guest, "/run/katl/boot-health/pending")
	fallbackBootID := guestBootID(t, ctx, client)
	guestCommand(t, ctx, guest, "deadman-skips-without-trial", "systemctl", "start", "katl-boot-deadman.service")
	if got := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "deadman-condition-after-fallback", "systemctl", "show", "--property=ConditionResult", "--value", "katl-boot-deadman.service")); got != "no" {
		t.Fatalf("deadman condition = %q, want no without an armed trial", got)
	}
	if got := guestBootID(t, ctx, client); got != fallbackBootID {
		t.Fatalf("condition-skipped deadman changed boot ID from %s to %s", fallbackBootID, got)
	}
	previousJournal := guestCommandOutput(t, ctx, guest, "failed-trial-recovery-journal", "journalctl", "-b", "-1", "--no-pager", "-u", "katl-boot-health.service", "-u", "katl-boot-recovery.service")
	if !strings.Contains(previousJournal, "katl-boot-recovery") || !strings.Contains(previousJournal, "rebootRequested=true") {
		t.Fatalf("failed trial journal does not prove automatic fallback:\n%s", previousJournal)
	}
	list := runKatlctl(t, ctx, result, katlctl, "generations-after-failed-trial", "node", "generations", "list", "cp-1")
	if !strings.Contains(string(list), candidate) || !strings.Contains(string(list), fallbackGeneration) {
		t.Fatalf("katlctl generation list missing recovery evidence:\n%s", list)
	}

	repeatBootID := guestBootID(t, ctx, client)
	runKatlctl(t, ctx, result, katlctl, "host-reboot-after-fallback", "node", "reboot", "cp-1", "--timeout", "3m")
	_ = client.Close()
	guest, client = reconnectGuestAfterGeneration(t, ctx, node, repeatBootID, fallbackGeneration)
	assertBootedGenerationIdentity(t, ctx, guest, fallbackSpec)
	assertGuestMissing(t, ctx, guest, "/run/katl/boot-health/pending")
	return guest, client
}

func runVolumeRemovalSmoke(t *testing.T, ctx context.Context, guest *GuestControl, result Result, katlctl katlctlRunner, endpoint string) string {
	t.Helper()
	addGeneration := "2026.06.06-vmtest-volume-add"
	removeGeneration := "2026.06.06-vmtest-volume-remove"
	readdGeneration := "2026.06.06-vmtest-volume-readd"

	added := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-volume-add", "live", addGeneration, configApplyFixture(t, "live-volume-add.yaml"), false)
	addedStatus := waitKatlcOperationTerminal(t, ctx, endpoint, added.OperationId)
	if addedStatus.Result != operation.ResultSucceeded || addedStatus.ConfigApplyPhase != "active" {
		t.Fatalf("volume add operation status = %+v, want active success", addedStatus)
	}
	guestCommand(t, ctx, guest, "volume-add-mounted", "findmnt", "--mountpoint", "/var/mnt/data")
	markerSource := "/var/lib/katl/test-artifacts/config-apply/volume-removal-marker"
	if _, err := guest.WriteFile(ctx, GuestFileRequest{
		Name:     "volume-removal-marker-source",
		Path:     markerSource,
		Content:  []byte("preserve me\n"),
		Mode:     0o600,
		Truncate: true,
	}); err != nil {
		t.Fatalf("write volume marker source: %v", err)
	}
	guestCommand(t, ctx, guest, "volume-write-marker", "install", "-m", "0600", markerSource, "/var/mnt/data/katl-removal-preserves-data")

	removed := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-volume-remove", "live", removeGeneration, configApplyFixture(t, "live-volume-remove.yaml"), false)
	removedStatus := waitKatlcOperationTerminal(t, ctx, endpoint, removed.OperationId)
	if removedStatus.Result != operation.ResultSucceeded || removedStatus.ConfigApplyPhase != "active" {
		t.Fatalf("volume removal operation status = %+v, want active success", removedStatus)
	}
	guestCommand(t, ctx, guest, "volume-removal-unmounted", "test", "!", "-e", "/var/mnt/data/katl-removal-preserves-data")
	guestCommand(t, ctx, guest, "volume-removal-unit-gone", "test", "!", "-e", "/etc/systemd/system/var-mnt-data.mount")
	guestCommand(t, ctx, guest, "volume-removal-partition-preserved", "test", "-b", "/dev/disk/by-partlabel/u-data")
	if filesystem := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "volume-removal-filesystem-preserved", "blkid", "-s", "TYPE", "-o", "value", "/dev/disk/by-partlabel/u-data")); filesystem != "xfs" {
		t.Fatalf("filesystem after volume removal = %q, want xfs", filesystem)
	}

	readded := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-volume-readd", "live", readdGeneration, configApplyFixture(t, "live-volume-readd.yaml"), false)
	readdedStatus := waitKatlcOperationTerminal(t, ctx, endpoint, readded.OperationId)
	if readdedStatus.Result != operation.ResultSucceeded || readdedStatus.ConfigApplyPhase != "active" {
		t.Fatalf("volume re-add operation status = %+v, want active success", readdedStatus)
	}
	guestCommand(t, ctx, guest, "volume-readd-mounted", "findmnt", "--mountpoint", "/var/mnt/data")
	guestCommand(t, ctx, guest, "volume-readd-marker-preserved", "test", "-f", "/var/mnt/data/katl-removal-preserves-data")
	assertGuestFileContains(t, ctx, guest, removed.RecordPath, `"operationKind": "generation-apply"`, `"result": "succeeded"`)

	return runPartitionWipeSmoke(t, ctx, guest, result, katlctl, endpoint)
}

func assertInstalledSSHReady(t *testing.T, ctx context.Context, guest *GuestControl) {
	t.Helper()
	guestCommand(t, ctx, guest, "sshd-active", "systemctl", "is-active", "--quiet", "sshd.service")
	guestCommand(t, ctx, guest, "persistent-ssh-host-key", "test", "-s", "/var/lib/katl/ssh/host-keys/ssh_host_ed25519_key")
	guestCommand(t, ctx, guest, "valid-persistent-ssh-host-key", "sshd", "-t")
	guestCommand(t, ctx, guest, "katl-authorized-key", "test", "-s", "/etc/ssh/authorized_keys/katl")
	guestCommand(t, ctx, guest, "root-authorized-key", "test", "-s", "/etc/ssh/authorized_keys/root")
	effective := strings.ToLower(guestCommandOutput(t, ctx, guest, "sshd-effective-config", "sshd", "-T"))
	for _, want := range []string{
		"authorizedkeysfile /etc/ssh/authorized_keys/%u",
		"hostkey /var/lib/katl/ssh/host-keys/ssh_host_ed25519_key",
		"permitrootlogin prohibit-password",
		"authenticationmethods publickey",
		"passwordauthentication no",
		"kbdinteractiveauthentication no",
		"gssapiauthentication no",
		"gssapikeyexchange no",
		"allowusers root",
		"allowusers katl",
	} {
		if !strings.Contains(effective, want) {
			t.Fatalf("effective sshd configuration missing %q:\n%s", want, effective)
		}
	}
}

func submitKatlctlConfigApply(t *testing.T, ctx context.Context, result Result, katlctl katlctlRunner, endpoint, name, mode, generationID, fixture string, wantFailure bool) agentapi.OperationAccepted {
	t.Helper()
	args := []string{
		"node", "apply",
		"--node", "cp-1",
	}
	args = append(args, configApplyConfigArgs(fixture)...)
	args = append(
		args,
		"--candidate-generation", generationID,
		"--actor", "installed-runtime config apply vmtest",
		"--output", "json",
	)
	if mode != "" {
		args = append(args, "--mode", mode)
	}
	output, commandErr := runKatlctlOutcome(t, ctx, result, katlctl, name, args...)
	if wantFailure && commandErr == nil {
		t.Fatalf("%s succeeded, want terminal operation failure\n%s", name, output)
	}
	if !wantFailure && commandErr != nil {
		t.Fatalf("%s: katlctl failed: %v\n%s", name, commandErr, output)
	}
	var terminal agentapi.OperationStatus
	mustUnmarshalProtoJSON(t, output, &terminal)
	if !terminal.Terminal || (wantFailure && terminal.Result == operation.ResultSucceeded) || (!wantFailure && terminal.Result != operation.ResultSucceeded) {
		t.Fatalf("%s terminal result = %+v", name, &terminal)
	}
	listOutput := runKatlctl(t, ctx, result, katlctl, name+"-operations",
		"operations", "list", "--endpoint", endpoint, "--limit", "20", "--output", "json")
	var listed agentapi.ListOperationsResponse
	mustUnmarshalProtoJSON(t, listOutput, &listed)
	for _, status := range listed.Operations {
		if status.CandidateGenerationId == generationID {
			return agentapi.OperationAccepted{
				OperationId:   status.OperationId,
				OperationKind: status.OperationKind,
				RecordPath:    "/var/lib/katl/operations/" + status.OperationId + "/record.json",
			}
		}
	}
	t.Fatalf("%s operation for generation %q not found in recent operations", name, generationID)
	return agentapi.OperationAccepted{}
}

func configApplyConfigArgs(path string) []string {
	return []string{"--config", path}
}

func katlctlGenerationStatus(t *testing.T, ctx context.Context, result Result, katlctl katlctlRunner, endpoint, name, generationID string) *agentapi.Generation {
	t.Helper()
	output := runKatlctl(
		t, ctx, result, katlctl, name,
		"node", "apply", "status",
		"--endpoint", endpoint,
		"--generation", generationID,
	)
	gen := &agentapi.Generation{}
	mustUnmarshalProtoJSON(t, output, gen)
	return gen
}

func restartGuestAndReconnect(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, guest *GuestControl, client *AgentClient) (*GuestControl, *AgentClient) {
	t.Helper()
	if node == nil || node.handle == nil {
		t.Fatal("running installed runtime node handle is required")
	}
	previousBootID := guestBootID(t, ctx, client)
	if err := requestGuestReboot(ctx, guest); err != nil {
		t.Fatalf("request guest reboot for staged generation restart: %v", err)
	}
	_ = client.Close()
	return reconnectGuestAfterBoot(t, ctx, node, previousBootID)
}

func guestBootID(t *testing.T, ctx context.Context, client *AgentClient) string {
	t.Helper()
	health, err := client.Health(ctx)
	if err != nil {
		t.Fatalf("vmtest agent health before staged generation restart: %v", err)
	}
	previousBootID := strings.TrimSpace(health.GetBootId())
	if previousBootID == "" {
		t.Fatal("vmtest agent health returned empty boot ID before staged generation restart")
	}
	return previousBootID
}

func reconnectGuestAfterBoot(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, previousBootID string) (*GuestControl, *AgentClient) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		next, err := DialAgent(dialCtx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
		cancel()
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			failIfRestartExited(t, node.handle)
			continue
		}
		healthCtx, healthCancel := context.WithTimeout(ctx, 5*time.Second)
		health, err := next.Health(healthCtx)
		healthCancel()
		if err != nil {
			lastErr = err
			_ = next.Close()
			time.Sleep(time.Second)
			failIfRestartExited(t, node.handle)
			continue
		}
		if bootID := strings.TrimSpace(health.GetBootId()); bootID != "" && bootID != previousBootID {
			guest := NewGuestControl(node.Result, next)
			if err := RunKatlcSmoke(ctx, guest); err != nil {
				_ = next.Close()
				t.Fatalf("katlc runtime smoke after staged generation restart: %v", err)
			}
			return guest, next
		}
		lastErr = errors.New("vmtest agent still reports previous boot ID")
		_ = next.Close()
		time.Sleep(time.Second)
		failIfRestartExited(t, node.handle)
	}
	t.Fatalf("vmtest agent did not reconnect with a new boot ID after staged generation restart: %v", lastErr)
	return nil, nil
}

func reconnectGuestAfterGeneration(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, previousBootID, generationID string) (*GuestControl, *AgentClient) {
	t.Helper()
	for attempt := 0; attempt < 2; attempt++ {
		guest, client := reconnectGuestAfterBoot(t, ctx, node, previousBootID)
		if currentGenerationFromGuest(t, ctx, guest) == generationID {
			return guest, client
		}
		previousBootID = guestBootID(t, ctx, client)
		_ = client.Close()
	}
	t.Fatalf("vmtest agent did not reconnect on generation %s after automatic recovery", generationID)
	return nil, nil
}

func powerOffGuestForCleanSuccess(t *testing.T, ctx context.Context, node *RunningInstalledRuntimeNode, guest *GuestControl, client *AgentClient) {
	t.Helper()
	if node == nil || node.handle == nil {
		t.Fatal("running installed runtime node handle is required")
	}
	if err := requestGuestPoweroff(ctx, guest); err != nil {
		t.Fatalf("request guest poweroff after config apply smoke success: %v", err)
	}
	if client != nil {
		_ = client.Close()
	}
	if err := waitGuestPoweredOff(node.handle, 45*time.Second); err != nil {
		t.Fatalf("wait for guest poweroff after config apply smoke success: %v", err)
	}
}

func shutdownGuestThroughKatlctl(t *testing.T, ctx context.Context, result Result, katlctl katlctlRunner, endpoint string, node *RunningInstalledRuntimeNode, client *AgentClient) {
	t.Helper()
	if node == nil || node.handle == nil {
		t.Fatal("running installed runtime node handle is required")
	}
	runKatlctl(
		t, ctx, result, katlctl, "host-shutdown",
		"node", "shutdown", "cp-1",
		"--timeout", "2m",
	)
	if client != nil {
		_ = client.Close()
	}
	if err := waitGuestPoweredOff(node.handle, 45*time.Second); err != nil {
		t.Fatalf("wait for guest shutdown after config apply smoke success: %v", err)
	}
}

func requestGuestPoweroff(ctx context.Context, guest *GuestControl) error {
	if guest == nil {
		return errors.New("guest control is required")
	}
	_, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name:    "poweroff-for-staged-generation",
		Argv:    []string{"systemd-run", "--quiet", "--collect", "--unit=katl-vmtest-poweroff-for-staged-generation", "--on-active=1s", "/usr/bin/systemctl", "poweroff", "--no-block"},
		Timeout: 10 * time.Second,
	})
	return err
}

func requestGuestReboot(ctx context.Context, guest *GuestControl) error {
	if guest == nil {
		return errors.New("guest control is required")
	}
	_, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name:    "reboot-for-staged-generation",
		Argv:    []string{"systemd-run", "--quiet", "--collect", "--unit=katl-vmtest-reboot-for-staged-generation", "--on-active=1s", "/usr/bin/systemctl", "reboot", "--no-block"},
		Timeout: 10 * time.Second,
	})
	return err
}

func waitGuestPoweredOff(handle *VMHandle, timeout time.Duration) error {
	if handle == nil || handle.done == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-handle.done:
		if err := handle.Wait(); err != nil {
			return err
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("guest did not power off within %s", timeout)
	}
}

func failIfRestartExited(t *testing.T, handle *VMHandle) {
	t.Helper()
	select {
	case <-handle.done:
		err := handle.Wait()
		t.Fatalf("runtime VM exited before vmtest agent reconnected after staged generation restart: %v", err)
	default:
	}
}

func runKatlctl(t *testing.T, ctx context.Context, result Result, katlctl katlctlRunner, name string, args ...string) []byte {
	t.Helper()
	stdout, err := runKatlctlOutcome(t, ctx, result, katlctl, name, args...)
	if err != nil {
		t.Fatalf("%s: katlctl failed: %v\nstdout:\n%s", name, err, stdout)
	}
	return stdout
}

func runKatlctlOutcome(t *testing.T, ctx context.Context, result Result, katlctl katlctlRunner, name string, args ...string) ([]byte, error) {
	t.Helper()
	return recordKatlctlOutcome(ctx, repoRoot(t), result, katlctl, name, args...)
}

func recordKatlctlOutcome(ctx context.Context, repo string, result Result, katlctl katlctlRunner, name string, args ...string) ([]byte, error) {
	dir := filepath.Join(result.RunDir, "katlctl", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create katlctl artifact dir: %w", err)
	}
	record := struct {
		Name  string   `json:"name"`
		Argv  []string `json:"argv"`
		Error string   `json:"error,omitempty"`
	}{
		Name: name,
		Argv: append([]string{"katlctl"}, args...),
	}
	var stdout, stderr bytes.Buffer
	cmd, err := katlctl.bin.Command(ctx, katlctl.config, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	cmd.Dir = repo
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		record.Error = err.Error()
	}
	if data, marshalErr := json.MarshalIndent(record, "", "  "); marshalErr == nil {
		_ = os.WriteFile(filepath.Join(dir, "command.json"), append(data, '\n'), 0o644)
	}
	_ = os.WriteFile(filepath.Join(dir, "stdout"), stdout.Bytes(), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "stderr"), stderr.Bytes(), 0o644)
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("%w\nstderr:\n%s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func configApplyFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), "internal", "vmtest", "testdata", "config-apply", name)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config apply fixture %s: %v", name, err)
	}
	return path
}

func mustUnmarshalProtoJSON(t *testing.T, data []byte, msg proto.Message) {
	t.Helper()
	if err := protojson.Unmarshal(data, msg); err != nil {
		t.Fatalf("decode proto json into %T: %v\n%s", msg, err, data)
	}
}

func waitKatlcOperationTerminal(t *testing.T, ctx context.Context, endpoint, operationID string, nodeNames ...string) *agentapi.OperationStatus {
	t.Helper()
	conn, client := dialKatlcAgentForVMTest(t, ctx, endpoint, nodeNames...)
	defer conn.Close()
	deadline := time.Now().Add(2 * time.Minute)
	var last *agentapi.OperationStatus
	for time.Now().Before(deadline) {
		status, err := client.GetOperation(ctx, &agentapi.GetOperationRequest{
			OperationId:        operationID,
			IncludeDiagnostics: "verbose",
		})
		if err != nil {
			t.Fatalf("get operation %s: %v", operationID, err)
		}
		last = status
		if status.Terminal {
			return status
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("operation %s did not become terminal; last status = %+v", operationID, last)
	return nil
}

func assertBootstrappedKubernetesSysextChangeRejected(t *testing.T, ctx context.Context, guest *GuestControl, endpoint string) {
	t.Helper()
	const kubeletConf = "/etc/kubernetes/kubelet.conf"
	const kubeletMarker = `apiVersion: v1
kind: Config
clusters: []
contexts: []
current-context: ""
users: []
`
	beforeSysext := readlinkOptional(t, ctx, guest, "/run/extensions/katl-kubernetes.raw")
	beforeGeneration := currentGenerationFromGuest(t, ctx, guest)
	markerSource := "/var/lib/katl/test-artifacts/config-apply/kubelet.conf"
	if _, err := guest.WriteFile(ctx, GuestFileRequest{
		Name:    "bootstrapped-kubelet-conf-source",
		Path:    markerSource,
		Content: []byte(kubeletMarker),
		Mode:    0o600,
	}); err != nil {
		t.Fatalf("write bootstrapped kubelet marker source: %v", err)
	}
	if _, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name: "install-bootstrapped-kubelet-conf",
		Argv: []string{"install", "-D", "-m", "0600", markerSource, kubeletConf},
	}); err != nil {
		t.Fatalf("install bootstrapped kubelet marker: %v", err)
	}
	assertGuestFileContains(t, ctx, guest, kubeletConf, "kind: Config")
	assertGuestMissing(t, ctx, guest, "/etc/kubernetes/admin.conf")
	assertGuestMissing(t, ctx, guest, "/etc/kubernetes/manifests/kube-apiserver.yaml")
	assertGuestMissing(t, ctx, guest, "/var/lib/kubelet/config.yaml")

	conn, client := dialKatlcAgentForVMTest(t, ctx, endpoint)
	defer conn.Close()
	nodeStatus, err := client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
	if err != nil {
		t.Fatalf("read enrolled node identity before Kubernetes sysext refusal: %v", err)
	}
	accepted, err := client.SubmitOperation(ctx, &agentapi.SubmitOperationRequest{
		ApiVersion:                  operation.APIVersion,
		Kind:                        agent.RequestKind,
		ClientRequestId:             "vmtest-kubeadm-upgrade-refused",
		OperationKind:               agent.OperationKindKubeadmUpgrade,
		Actor:                       "installed-runtime config apply vmtest",
		ExpectedEnrollmentId:        nodeStatus.GetEnrollmentId(),
		ExpectedInventoryNodeName:   nodeStatus.GetInventoryNodeName(),
		ExpectedMachineId:           nodeStatus.GetMachineId(),
		ExpectedCurrentGenerationId: nodeStatus.GetCurrentGenerationId(),
		KubernetesSysextUpdate: &agentapi.KubernetesSysextUpdateOperationRequest{
			TargetPayloadVersion: "v9.99.0",
			TargetSysextPath:     "/var/lib/katl/artifacts/katlos-image/katl-kubernetes.raw",
			TargetSysextSha256:   strings.Repeat("e", 64),
		},
	})
	if beforeSysext == "" {
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "has no selected Kubernetes sysext") {
			t.Fatalf("submit Kubernetes sysext update without a selected sysext: %v, want fail-closed precondition", err)
		}
		assertGuestFileContains(t, ctx, guest, kubeletConf, "kind: Config")
		assertGuestMissing(t, ctx, guest, "/etc/kubernetes/admin.conf")
		assertGuestMissing(t, ctx, guest, "/etc/kubernetes/manifests/kube-apiserver.yaml")
		assertGuestMissing(t, ctx, guest, "/var/lib/kubelet/config.yaml")
		if got := currentGenerationFromGuest(t, ctx, guest); got != beforeGeneration {
			t.Fatalf("current generation after rejected sysext update = %q, want %q", got, beforeGeneration)
		}
		return
	}
	if err != nil {
		t.Fatalf("submit Kubernetes sysext update rejection request: %v", err)
	}
	status := accepted.GetInitialStatus()
	if !status.GetTerminal() {
		status = waitKatlcOperationTerminal(t, ctx, endpoint, accepted.OperationId)
	}
	if status.GetOperationKind() != agent.OperationKindKubeadmUpgrade ||
		status.GetPhase() != "execution-refused-unsupported" ||
		status.GetResult() != "execution-refused-unsupported" ||
		!status.GetTerminal() ||
		status.GetExternalMutationStarted() ||
		len(status.GetMutationScopes()) != 0 ||
		len(status.GetInvocations()) != 0 ||
		strings.TrimSpace(status.GetCandidateGenerationId()) != "" {
		t.Fatalf("Kubernetes sysext rejection status = %+v", status)
	}
	if !strings.Contains(status.GetFailureReason(), "target kubeadm access mode is not selected") ||
		!strings.Contains(status.GetNextAction(), "kubelet activation gate") {
		t.Fatalf("Kubernetes sysext rejection diagnostics = failure %q next %q", status.GetFailureReason(), status.GetNextAction())
	}
	assertGuestFileContains(t, ctx, guest, accepted.RecordPath, `"operationKind": "kubeadm-upgrade"`, `"phase": "execution-refused-unsupported"`)
	assertGuestFileContains(t, ctx, guest, kubeletConf, "kind: Config")
	assertGuestMissing(t, ctx, guest, "/etc/kubernetes/admin.conf")
	assertGuestMissing(t, ctx, guest, "/etc/kubernetes/manifests/kube-apiserver.yaml")
	assertGuestMissing(t, ctx, guest, "/var/lib/kubelet/config.yaml")
	assertOptionalReadlink(t, ctx, guest, "/run/extensions/katl-kubernetes.raw", beforeSysext)
	if got := currentGenerationFromGuest(t, ctx, guest); got != beforeGeneration {
		t.Fatalf("current generation after rejected sysext update = %q, want %q", got, beforeGeneration)
	}
}

func dialKatlcAgentForVMTest(t *testing.T, ctx context.Context, endpoint string, nodeNames ...string) (*grpc.ClientConn, agentapi.KatlcAgentClient) {
	t.Helper()
	nodeName := "cp-1"
	if len(nodeNames) > 0 && strings.TrimSpace(nodeNames[0]) != "" {
		nodeName = strings.TrimSpace(nodeNames[0])
	}
	identity, err := VMTestManagementIdentity(VMTestManagementClusterName)
	if err != nil {
		t.Fatal(err)
	}
	client, err := managementidentity.Client(identity, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := transport.ClientTLSConfig(client, nodeName)
	if err != nil {
		t.Fatal(err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, endpoint, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithBlock())
	if err != nil {
		t.Fatalf("dial katlc agent %s: %v", endpoint, err)
	}
	return conn, agentapi.NewKatlcAgentClient(conn)
}

func configApplyFailureDiagnostics() GuestDiagnostics {
	return GuestDiagnostics{
		Timeout: 2 * time.Minute,
		Journals: []GuestJournalRequest{
			{Name: "katlc-agent-journal", Units: []string{"katlc-agent.service"}, MaxBytes: 512 << 10},
		},
		Commands: []GuestCommandRequest{
			{Name: "katlc-agent-status", Argv: []string{"systemctl", "status", "--no-pager", "katlc-agent.service"}, AllowFailure: true},
			{Name: "generation-json-files", Argv: []string{"find", "/var/lib/katl/generations", "-maxdepth", "3", "-type", "f", "-name", "*.json", "-print"}, AllowFailure: true},
			{Name: "operation-json-files", Argv: []string{"find", "/var/lib/katl/operations", "-maxdepth", "3", "-type", "f", "-name", "*.json", "-print"}, AllowFailure: true},
		},
	}
}

func currentGenerationFromGuest(t *testing.T, ctx context.Context, guest *GuestControl) string {
	t.Helper()
	cmdline := readGuestFile(t, ctx, guest, "/proc/cmdline")
	for _, field := range strings.Fields(cmdline) {
		if value, ok := strings.CutPrefix(field, "katl.generation="); ok && value != "" {
			return value
		}
	}
	t.Fatalf("guest /proc/cmdline has no katl.generation: %s", cmdline)
	return ""
}

func readlink(t *testing.T, ctx context.Context, guest *GuestControl, path string) string {
	t.Helper()
	record, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name: "readlink",
		Argv: []string{"readlink", path},
	})
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	return strings.TrimSpace(readFile(t, record.Stdout))
}

func readlinkOptional(t *testing.T, ctx context.Context, guest *GuestControl, path string) string {
	t.Helper()
	record, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name:         "readlink-" + filepath.Base(path),
		Argv:         []string{"readlink", path},
		AllowFailure: true,
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(readFile(t, record.Stdout))
}

func assertReadlink(t *testing.T, ctx context.Context, guest *GuestControl, path, want string) {
	t.Helper()
	if got := readlink(t, ctx, guest, path); got != want {
		t.Fatalf("readlink %s = %q, want %q", path, got, want)
	}
}

func assertOptionalReadlink(t *testing.T, ctx context.Context, guest *GuestControl, path, want string) {
	t.Helper()
	if want == "" {
		assertGuestMissing(t, ctx, guest, path)
		return
	}
	assertReadlink(t, ctx, guest, path, want)
}

func readGuestFile(t *testing.T, ctx context.Context, guest *GuestControl, path string) string {
	t.Helper()
	record, err := guest.ReadFile(ctx, GuestFileRequest{
		Name:         filepath.Base(path),
		Path:         path,
		StoreContent: true,
	})
	if err != nil {
		t.Fatalf("read guest file %s: %v", path, err)
	}
	return readFile(t, record.Artifact)
}

func assertGuestNonLoopbackLink(t *testing.T, ctx context.Context, guest *GuestControl) {
	t.Helper()
	output := guestCommandOutput(t, ctx, guest, "ip-links-before-networkd-stage", "ip", "-o", "link", "show")
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, ": lo:") {
			continue
		}
		return
	}
	t.Fatalf("guest has no non-loopback links before networkd staged apply:\n%s", output)
}

func assertGuestAddress(t *testing.T, ctx context.Context, guest *GuestControl, address string, prefix int) {
	t.Helper()
	want := address + "/" + strconv.Itoa(prefix)
	deadline := time.Now().Add(90 * time.Second)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		record, err := guest.RunCommand(ctx, GuestCommandRequest{
			Name: "ip-addresses",
			Argv: []string{"ip", "-o", "-4", "address", "show"},
		})
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		last = readFile(t, record.Stdout)
		if strings.Contains(last, want) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("read guest IPv4 addresses while waiting for %s: %v", want, lastErr)
	}
	links := guestCommandOutput(t, ctx, guest, "ip-links-after-address-timeout", "ip", "-o", "link", "show")
	t.Fatalf("guest IPv4 addresses missing %s before timeout:\naddresses:\n%s\nlinks:\n%s", want, last, links)
}

func waitGuestFileContains(t *testing.T, ctx context.Context, guest *GuestControl, path string, wants ...string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		record, err := guest.ReadFile(ctx, GuestFileRequest{
			Name:         filepath.Base(path),
			Path:         path,
			StoreContent: true,
		})
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		last = readFile(t, record.Artifact)
		if containsAll(last, wants...) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("read guest file %s while waiting: %v", path, lastErr)
	}
	t.Fatalf("%s did not contain %q before timeout:\n%s", path, wants, last)
}

func assertGuestFileContains(t *testing.T, ctx context.Context, guest *GuestControl, path string, wants ...string) {
	t.Helper()
	content := readGuestFile(t, ctx, guest, path)
	for _, want := range wants {
		if !strings.Contains(content, want) {
			t.Fatalf("%s missing %q:\n%s", path, want, content)
		}
	}
}

func containsAll(content string, wants ...string) bool {
	for _, want := range wants {
		if !strings.Contains(content, want) {
			return false
		}
	}
	return true
}

func assertGuestExists(t *testing.T, ctx context.Context, guest *GuestControl, path string) {
	t.Helper()
	if _, err := guest.RunCommand(ctx, GuestCommandRequest{Name: filepath.Base(path), Argv: []string{"test", "-e", path}}); err != nil {
		t.Fatalf("guest path %s missing: %v", path, err)
	}
}

func assertGuestMissing(t *testing.T, ctx context.Context, guest *GuestControl, path string) {
	t.Helper()
	if record, err := guest.RunCommand(ctx, GuestCommandRequest{Name: filepath.Base(path), Argv: []string{"test", "!", "-e", path}}); err != nil {
		t.Fatalf("guest path %s exists or could not be checked: %v %#v", path, err, record)
	}
}

func assertMaskedUnits(t *testing.T, ctx context.Context, guest *GuestControl, phase string) {
	t.Helper()
	for _, unit := range []string{"systemd-userdbd.service", "systemd-userdbd.socket"} {
		for property, want := range map[string]string{"LoadState": "masked", "ActiveState": "inactive"} {
			got := strings.TrimSpace(guestCommandOutput(t, ctx, guest, phase+"-"+unit+"-"+property, "systemctl", "show", "--property="+property, "--value", unit))
			if got != want {
				t.Fatalf("%s %s %s = %q, want %q", phase, unit, property, got, want)
			}
		}
		started, err := guest.RunCommand(ctx, GuestCommandRequest{Name: phase + "-start-masked-" + unit, Argv: []string{"systemctl", "start", unit}, AllowFailure: true})
		if err != nil || started.ExitStatus == 0 {
			t.Fatalf("masked %s started: result=%+v err=%v", unit, started, err)
		}
	}
}

// The consumer sorts before its producer. Only systemd ordering, rather than
// invocation order, can make both first activation and subsequent changes work.
func runSystemdUnitSmoke(t *testing.T, ctx context.Context, guest *GuestControl, client *AgentClient, result Result, katlctl katlctlRunner, endpoint string) {
	t.Helper()
	base, err := os.ReadFile(configApplyFixture(t, "live-udev.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	beforeBoot := guestBootID(t, ctx, client)
	var lastConfig string
	var timePID string
	for version, value := range []string{"first", "second"} {
		config := strings.Replace(string(base), "      sets:\n", `      enabledUnits: [a-consumer.service, z-producer.service, systemd-timesyncd.service]
      sets:
        ordered-units:
          files:
            - path: /etc/systemd/system/z-producer.service
              content: |
                [Service]
                Type=oneshot
                RemainAfterExit=yes
                ExecStart=/usr/bin/sh -c 'echo `+value+` > /run/unit-order'
                [Install]
                WantedBy=multi-user.target
            - path: /etc/systemd/system/a-consumer.service
              content: |
                [Unit]
                After=z-producer.service
                [Service]
                Type=oneshot
                RemainAfterExit=yes
                ExecStart=/usr/bin/sh -c 'test "$(cat /run/unit-order)" = "`+value+`"'
                [Install]
                WantedBy=multi-user.target
            - path: /etc/systemd/timesyncd.conf.d/80-katl-vmtest.conf
              content: |
                [Time]
                PollIntervalMinSec=`+strconv.Itoa(version+30)+`s
`, 1)
		config = strings.Replace(config, "sourceID: operator", "sourceID: systemd-journey", 1)
		config = strings.Replace(config, `desiredVersion: "21"`, fmt.Sprintf("desiredVersion: %q", strconv.Itoa(version+1)), 1)
		path := filepath.Join(t.TempDir(), "units.yaml")
		lastConfig = config
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "systemd-"+value, "live", "systemd-"+value, path, false)
		for _, unit := range []string{"a-consumer.service", "z-producer.service", "systemd-timesyncd.service"} {
			guestCommand(t, ctx, guest, "active-"+value+"-"+unit, "systemctl", "is-active", "--quiet", unit)
		}
		if got := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "producer-output-"+value, "systemd-run", "--quiet", "--wait", "--collect", "--pipe", "/usr/bin/cat", "/run/unit-order")); got != value {
			t.Fatalf("producer output = %q, want %q", got, value)
		}
		if got := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "native-enable-"+value, "systemctl", "show", "--property=UnitFileState", "--value", "systemd-timesyncd.service")); got != "enabled-runtime" {
			t.Fatalf("time service enablement = %q", got)
		}
		if got := guestCommandOutput(
			t, ctx, guest, "time-config-"+value,
			"systemd-run", "--quiet", "--wait", "--collect", "--pipe",
			"/usr/bin/systemd-analyze", "cat-config", "systemd/timesyncd.conf",
		); !strings.Contains(got, "PollIntervalMinSec="+strconv.Itoa(version+30)+"s") {
			t.Fatalf("timesyncd configuration does not include the applied drop-in:\n%s", got)
		}
		pid := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "time-pid-"+value, "systemctl", "show", "--property=MainPID", "--value", "systemd-timesyncd.service"))
		if timePID != "" && pid == timePID {
			t.Fatalf("timesyncd PID remained %s after its drop-in changed", pid)
		}
		timePID = pid
	}
	rejected := strings.ReplaceAll(lastConfig, "second", "rejected")
	rejected = strings.Replace(rejected, `desiredVersion: "2"`, `desiredVersion: "3"`, 1)
	rejected = strings.Replace(rejected, "ExecStart=/usr/bin/sh -c 'echo rejected > /run/unit-order'", "ExecStart=/usr/bin/false", 1)
	path := filepath.Join(t.TempDir(), "failed-units.yaml")
	if err := os.WriteFile(path, []byte(rejected), 0o600); err != nil {
		t.Fatal(err)
	}
	accepted := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "systemd-failure", "live", "systemd-failure", path, true)
	if status := waitKatlcOperationTerminal(t, ctx, endpoint, accepted.OperationId); status.Result != operation.ResultFailedRolledBack {
		t.Fatalf("failed units result = %s, want restored configuration and services", status.Result)
	}
	guestCommand(t, ctx, guest, "rollback-units-active", "systemctl", "is-active", "--quiet", "a-consumer.service", "z-producer.service")
	if got := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "rollback-producer-output", "systemd-run", "--quiet", "--wait", "--collect", "--pipe", "/usr/bin/cat", "/run/unit-order")); got != "second" {
		t.Fatalf("rollback producer output = %q", got)
	}
	removed := strings.Replace(string(base), "sourceID: operator", "sourceID: systemd-journey", 1)
	removed = strings.Replace(removed, `desiredVersion: "21"`, `desiredVersion: "4"`, 1)
	path = filepath.Join(t.TempDir(), "removed-units.yaml")
	if err := os.WriteFile(path, []byte(removed), 0o600); err != nil {
		t.Fatal(err)
	}
	submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "systemd-remove", "live", "systemd-remove", path, false)
	for _, unit := range []string{"a-consumer.service", "z-producer.service", "systemd-timesyncd.service"} {
		if got := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "stopped-"+unit, "systemctl", "show", "--property=ActiveState", "--value", unit)); got != "inactive" {
			t.Fatalf("removed %s state = %q", unit, got)
		}
	}
	assertGuestMissing(t, ctx, guest, "/run/systemd/system/sysinit.target.wants/systemd-timesyncd.service")
	if afterBoot := guestBootID(t, ctx, client); afterBoot != beforeBoot {
		t.Fatal("unit changes rebooted the node")
	}
}

// runPartitionWipeSmoke applies a destructive volume to a partition that
// already exists, so the filesystem is created through the partition's
// by-diskseq link under real udev, and checks the result through the guest's
// own view of the partition. Applying the same volume again must preserve it.
// It returns the generation left active.
func runPartitionWipeSmoke(t *testing.T, ctx context.Context, guest *GuestControl, result Result, katlctl katlctlRunner, endpoint string) string {
	t.Helper()
	const wipeGeneration = "2026.06.06-vmtest-partition-wipe"
	const (
		disk      = "/dev/disk/by-id/virtio-katl-scratch"
		partition = "/dev/disk/by-partlabel/u-scratch"
		table     = "/var/lib/katl/test-artifacts/config-apply/scratch-partition-table"
	)
	if _, err := guest.WriteFile(ctx, GuestFileRequest{
		Name:     "scratch-partition-table-source",
		Path:     table,
		Content:  []byte("label: gpt\nname=u-scratch, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4\n"),
		Mode:     0o600,
		Truncate: true,
	}); err != nil {
		t.Fatalf("write scratch partition table: %v", err)
	}
	// The guest command runner has no stdin, so sfdisk reads its script from a
	// transient unit. --pipe is omitted because it replaces the unit's
	// StandardInput with the caller's, which leaves sfdisk without the script.
	guestCommand(
		t, ctx, guest, "scratch-partition-create",
		"systemd-run", "--wait", "--collect",
		"--property=StandardInput=file:"+table,
		"--property=StandardOutput=file:"+table+".log", "--property=StandardError=inherit",
		"sfdisk", disk,
	)
	guestCommand(t, ctx, guest, "scratch-partition-settled", "udevadm", "settle", "--timeout=30")
	if existing := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "scratch-partition-unformatted", "blkid", "-s", "TYPE", "-o", "value", partition)); existing != "" {
		t.Fatalf("scratch partition already has filesystem %q before the volume is applied", existing)
	}

	applied := submitKatlctlConfigApply(t, ctx, result, katlctl, endpoint, "config-apply-partition-wipe", "live", wipeGeneration, configApplyFixture(t, "live-volume-partition-wipe.yaml"), false)
	appliedStatus := waitKatlcOperationTerminal(t, ctx, endpoint, applied.OperationId)
	if appliedStatus.Result != operation.ResultSucceeded || appliedStatus.ConfigApplyPhase != "active" {
		t.Fatalf("partition wipe operation status = %+v, want active success", appliedStatus)
	}
	if filesystem := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "scratch-filesystem", "blkid", "-s", "TYPE", "-o", "value", partition)); filesystem != "xfs" {
		t.Fatalf("filesystem on the selected partition = %q, want xfs", filesystem)
	}
	if mounted := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "scratch-mounted", "findmnt", "--noheadings", "--output", "FSTYPE", "--mountpoint", "/var/mnt/scratch")); mounted != "xfs" {
		t.Fatalf("/var/mnt/scratch filesystem = %q, want xfs", mounted)
	}
	uuid := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "scratch-uuid", "blkid", "-s", "UUID", "-o", "value", partition))
	if uuid == "" {
		t.Fatalf("selected partition has no filesystem UUID after the volume was applied")
	}

	markerSource := "/var/lib/katl/test-artifacts/config-apply/scratch-marker"
	if _, err := guest.WriteFile(ctx, GuestFileRequest{Name: "scratch-marker-source", Path: markerSource, Content: []byte("keep\n"), Mode: 0o600, Truncate: true}); err != nil {
		t.Fatalf("write scratch marker source: %v", err)
	}
	guestCommand(t, ctx, guest, "scratch-write-marker", "install", "-m", "0600", markerSource, "/var/mnt/scratch/katl-reapply-preserves-data")

	// An unchanged desired state completes without creating a generation, so the
	// terminal status comes from the apply itself.
	var again agentapi.OperationStatus
	mustUnmarshalProtoJSON(t, runKatlctl(
		t, ctx, result, katlctl, "config-apply-partition-wipe-again",
		"node", "apply", "--node", "cp-1", "--config", configApplyFixture(t, "live-volume-partition-wipe.yaml"),
		"--candidate-generation", "2026.06.06-vmtest-partition-wipe-again",
		"--actor", "installed-runtime config apply vmtest", "--output", "json", "--mode", "live",
	), &again)
	if !again.Terminal || again.Result != operation.ResultSucceeded || again.ExternalMutationStarted {
		t.Fatalf("repeated partition wipe status = %+v, want terminal success without mutation", &again)
	}
	if after := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "scratch-uuid-after-reapply", "blkid", "-s", "UUID", "-o", "value", partition)); after != uuid {
		t.Fatalf("repeated apply reformatted the partition: filesystem UUID %s became %s", uuid, after)
	}
	guestCommand(t, ctx, guest, "scratch-marker-preserved", "test", "-f", "/var/mnt/scratch/katl-reapply-preserves-data")
	return wipeGeneration
}
