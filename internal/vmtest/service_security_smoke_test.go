package vmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/apiproxy"
)

func TestInstalledRuntimeServiceSecuritySmoke(t *testing.T) {
	options := DefaultOptions()
	if !options.Enabled {
		t.Skip("run with scripts/vmtest-run to exercise installed service restrictions")
	}
	world, ok := installedRuntimeWorldRunFor(t, "installed-runtime-service-security", NodeSpec{Name: "cp-1", Role: ControlPlane})
	if !ok {
		_ = RequireWorld(t)
		t.Fatal("installed runtime world is required")
	}
	scenario := Scenario{Name: "installed-runtime-service-security"}
	result, err := world.Runner.Plan(scenario)
	if err != nil {
		t.Fatal(err)
	}
	result = requirePlannedVMHost(t, world.Runner, scenario, result, HostRequirements{Libvirt: true, OVMF: true, KVM: options.KVM})

	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	runtime := world.Config
	runtime.VM.KVM = options.KVM
	runtime.VM.RAMMiB, runtime.VM.CPUs = 2048, 2
	runtime.VM.Timeout = 10 * time.Minute
	runtime.VM.Network.MAC = world.Node.MACAddress
	runtime.VM.VSock.Enabled = true
	runtime.VM.Agent.RequireHealth = true
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{Name: "cp-1", Runtime: runtime}, VMRunner{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			_ = node.StopFailure("service security smoke failed")
		} else {
			_ = node.Stop()
		}
	}()
	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	guest := NewGuestControl(node.Result, client)
	if err := guest.WaitBootComplete(ctx); err != nil {
		t.Fatal(err)
	}

	// Host orchestration and container mounts must remain visible to PID 1.
	hostNamespace := guestCommandOutput(t, ctx, guest, "host-mount-namespace", "readlink", "/proc/1/ns/mnt")
	for _, unit := range []string{"katlc-agent.service", "containerd.service"} {
		guestCommand(t, ctx, guest, unit+"-start", "systemctl", "start", unit)
		pid := strings.TrimSpace(guestCommandOutput(t, ctx, guest, unit+"-pid", "systemctl", "show", "--property=MainPID", "--value", unit))
		namespace := guestCommandOutput(t, ctx, guest, unit+"-mount-namespace", "readlink", "/proc/"+pid+"/ns/mnt")
		if namespace != hostNamespace {
			t.Fatalf("%s has a private mount namespace", unit)
		}
	}

	probePath := filepath.Join(t.TempDir(), "service-security")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", probePath, "./internal/vmtest/testdata/service-security")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build security probe: %v: %s", err, output)
	}
	probe, err := os.ReadFile(probePath)
	if err != nil {
		t.Fatal(err)
	}
	const guestProbe = "/var/lib/katl/test-artifacts/service-security"
	for offset := 0; offset < len(probe); offset += 512 << 10 {
		end := min(offset+(512<<10), len(probe))
		if _, err := guest.WriteFile(ctx, GuestFileRequest{Path: guestProbe, Content: probe[offset:end], Mode: 0o755, Offset: uint64(offset), Truncate: offset == 0}); err != nil {
			t.Fatal(err)
		}
	}
	writeGuestFile(t, ctx, guest, "/var/lib/katl/test-artifacts/security-secret", []byte("root credential fixture"), 0o600)
	config, err := json.Marshal(apiproxy.Config{
		TLSName: "127.0.0.1", CanonicalEndpoint: "127.0.0.1:6443",
		Listeners: []apiproxy.Listener{{Address: "127.0.0.1:7445", Exposure: apiproxy.ExposureNodeLocal}},
		Backends:  []apiproxy.Backend{{Name: "local", Address: "127.0.0.1:6443", Local: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	const proxyConfig = "/var/lib/katl/test-artifacts/security-proxy.json"
	writeGuestFile(t, ctx, guest, proxyConfig, config, 0o644)

	// Native application units must not inherit policy from a Katl name prefix.
	var nativePolicy string
	for _, unit := range []string{"vmtest-native-policy", "katl-app-vmtest-native-policy"} {
		observed := guestCommandOutput(t, ctx, guest, unit, "systemd-run", "--quiet", "--wait", "--pipe", "--collect", "--unit="+unit, "--", guestProbe, "native", "/tmp")
		if nativePolicy != "" && observed != nativePolicy {
			t.Fatalf("application policy depends on unit name: %q != %q", observed, nativePolicy)
		}
		nativePolicy = observed
	}

	for _, role := range []string{"console", "proxy"} {
		unit, directory := "katl-console.service", "/run/katl/console"
		content := "[Service]\n"
		if role == "proxy" {
			unit, directory = "katl-api-proxy.service", "/run/katl/api-proxy"
			content = "[Unit]\nConditionPathExists=\n\n[Service]\nExecStart=\nExecStart=/usr/lib/katl/runtime/katl-api-proxy --config=" + proxyConfig + "\n"
		}
		content += fmt.Sprintf("ExecStartPost=%s %s %s\n", guestProbe, role, directory)
		source := "/var/lib/katl/test-artifacts/" + role + ".conf"
		writeGuestFile(t, ctx, guest, source, []byte(content), 0o644)
		guestCommand(t, ctx, guest, role+"-probe-install", "systemd-run", "--wait", "--pipe", "--collect", "--", "install", "-D", "-m", "0644", source, "/run/systemd/system/"+unit+".d/90-vmtest.conf")
		guestCommand(t, ctx, guest, role+"-reload", "systemctl", "daemon-reload")
		guestCommand(t, ctx, guest, role+"-probe", "systemctl", "restart", unit)
		guestCommand(t, ctx, guest, role+"-active", "systemctl", "is-active", "--quiet", unit)
	}
	guestCommand(t, ctx, guest, "console-rendered", "test", "-s", "/run/katl/console/rendered.txt")
	guestCommand(t, ctx, guest, "no-unexpected-write", "test", "!", "-e", "/var/lib/katl/test-artifacts/security-unexpected")
	privileged := guestCommandOutput(t, ctx, guest, "privileged-executables", "find", "/usr", "-xdev", "-type", "f", "-perm", "/6000", "-print")
	if strings.TrimSpace(privileged) != "" {
		t.Fatalf("base executables grant setuid/setgid privileges:\n%s", privileged)
	}
	capabilities := guestCommandOutput(t, ctx, guest, "executable-capabilities", "systemd-run", "--quiet", "--wait", "--pipe", "--collect", "--", "/usr/sbin/getcap", "-r", "/usr")
	if strings.TrimSpace(capabilities) != "" {
		t.Fatalf("base executables grant file capabilities:\n%s", capabilities)
	}

	node.Result.finish(StatusPassed, "", world.Runner.time())
	if err := world.Runner.Write(scenario, node.Result); err != nil {
		t.Fatal(err)
	}
	if err := world.Scenario.WriteResult(WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}
