package bgpapivip

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

const BirdControlSocketPath = "/run/katl-bird/bird.ctl"

type CommandRunner interface {
	Output(context.Context, string, ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type CommandBirdClient struct {
	Runner CommandRunner
	Birdc  string
	Socket string
	Config Config
}

func (c CommandBirdClient) Status(ctx context.Context) (BirdRuntimeStatus, error) {
	output, err := c.run(ctx, "show", "protocols")
	status := BirdRuntimeStatus{
		ProcessActive:      err == nil,
		ControlSocketReady: err == nil,
		ControlSocketPath:  c.socket(),
		ReadinessState:     "not-ready",
	}
	if err != nil {
		status.FailureReason = boundedCommandFailure(output, err)
		return status, fmt.Errorf("query endpoint routing status: %s", status.FailureReason)
	}
	status.ReadinessState = "ready"
	status.Peers = parseProtocolStatus(string(output), c.Config)
	return status, nil
}

func (c CommandBirdClient) SetAdvertisement(ctx context.Context, enabled bool) error {
	action := "disable"
	if enabled {
		action = "enable"
	}
	output, err := c.run(ctx, action, "katl_api")
	if err != nil {
		return fmt.Errorf("%s endpoint route: %s", action, boundedCommandFailure(output, err))
	}
	return nil
}

func (c CommandBirdClient) run(ctx context.Context, args ...string) ([]byte, error) {
	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	birdc := strings.TrimSpace(c.Birdc)
	if birdc == "" {
		birdc = "birdc"
	}
	command := []string{"-s", c.socket()}
	command = append(command, args...)
	return runner.Output(ctx, birdc, command...)
}

func (c CommandBirdClient) socket() string {
	if strings.TrimSpace(c.Socket) != "" {
		return strings.TrimSpace(c.Socket)
	}
	return BirdControlSocketPath
}

type LinuxInterfaceChecker struct{}

func (LinuxInterfaceChecker) Ready(_ context.Context, config Config) (bool, error) {
	iface, err := net.InterfaceByName(config.VIPInterface.Name)
	if err != nil {
		return false, nil
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return false, fmt.Errorf("inspect endpoint interface: %w", err)
	}
	want := strings.SplitN(config.Endpoint.VIP, "/", 2)[0]
	for _, address := range addresses {
		if strings.SplitN(address.String(), "/", 2)[0] == want {
			return true, nil
		}
	}
	return false, nil
}

func parseProtocolStatus(output string, config Config) []PeerRuntimeStatus {
	known := map[string]PeerRuntimeStatus{}
	for _, peer := range config.FabricPeers {
		known[protocolName(peer)] = PeerRuntimeStatus{
			Name:          peer.Address,
			Kind:          "fabric",
			AddressFamily: config.Endpoint.AddressFamily,
			AdminState:    "unknown",
			SessionState:  "unknown",
		}
	}
	for _, exchange := range config.RouteExchange {
		known["katl_exchange_"+safeSymbol(exchange.Name)] = PeerRuntimeStatus{
			Name:          exchange.Name,
			Kind:          "route-exchange",
			AddressFamily: "ipv4",
			AdminState:    "unknown",
			SessionState:  "unknown",
		}
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		peer, ok := known[fields[0]]
		if !ok {
			continue
		}
		peer.AdminState = strings.ToLower(fields[3])
		peer.SessionState = strings.ToLower(strings.Join(fields[5:], "-"))
		known[fields[0]] = peer
	}
	out := make([]PeerRuntimeStatus, 0, len(known))
	for _, peer := range config.FabricPeers {
		out = append(out, known[protocolName(peer)])
	}
	for _, exchange := range config.RouteExchange {
		out = append(out, known["katl_exchange_"+safeSymbol(exchange.Name)])
	}
	return out
}

func boundedCommandFailure(output []byte, err error) string {
	message := strings.TrimSpace(string(output))
	if len(message) > 1024 {
		message = message[:1024]
	}
	if message == "" {
		message = err.Error()
	}
	return message
}
