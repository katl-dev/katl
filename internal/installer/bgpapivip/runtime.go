package bgpapivip

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	BirdControlSocketPath = "/run/katl-bird/bird.ctl"
	BirdExecutablePath    = "/usr/lib/katl/endpoint-routing/bird"
	BirdClientPath        = "/usr/lib/katl/endpoint-routing/birdc"
)

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
	output, err := c.run(ctx, "show", "protocols", "all")
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
	routeOutput, routeErr := c.run(ctx, "show", "route", "table", "katl_fabric", "for", c.Config.Endpoint.VIP, "protocol", "katl_api")
	if routeErr != nil {
		if routeIsAbsent(string(routeOutput)) {
			return status, nil
		}
		status.FailureReason = boundedCommandFailure(routeOutput, routeErr)
		return status, fmt.Errorf("query endpoint route status: %s", status.FailureReason)
	}
	status.RouteOriginated = routeIsOriginated(string(routeOutput), c.Config.Endpoint.VIP)
	for _, peer := range status.Peers {
		if status.RouterID == "" && peer.LocalAddress != "" {
			status.RouterID = peer.LocalAddress
		}
	}
	if c.Config.Routing.RouterID != "" {
		status.RouterID = c.Config.Routing.RouterID
	}
	return status, nil
}

func (c CommandBirdClient) run(ctx context.Context, args ...string) ([]byte, error) {
	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	birdc := strings.TrimSpace(c.Birdc)
	if birdc == "" {
		birdc = BirdClientPath
	}
	command := []string{"-s", c.socket()}
	command = append(command, args...)
	return runner.Output(ctx, birdc, command...)
}

func routeIsOriginated(output, prefix string) bool {
	return strings.Contains(output, prefix) && strings.Contains(output, "[katl_api ")
}

func routeIsAbsent(output string) bool {
	return strings.Contains(output, "Network not found")
}

func (c CommandBirdClient) socket() string {
	if strings.TrimSpace(c.Socket) != "" {
		return strings.TrimSpace(c.Socket)
	}
	return BirdControlSocketPath
}

type LinuxInterfaceChecker struct{}

func (LinuxInterfaceChecker) Ready(_ context.Context, config Config) (bool, error) {
	_, err := net.InterfaceByName(config.VIPInterface.Name)
	if err != nil {
		return false, nil
	}
	return true, nil
}

type NetlinkVIPOwner struct{}

func (NetlinkVIPOwner) Owned(ctx context.Context, config Config) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	iface, err := net.InterfaceByName(config.VIPInterface.Name)
	if err != nil {
		return false, nil
	}
	address, err := netip.ParsePrefix(config.Endpoint.VIP)
	if err != nil {
		return false, fmt.Errorf("parse endpoint address: %w", err)
	}
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_UNSPEC)
	if err != nil {
		return false, fmt.Errorf("list route netlink addresses: %w", err)
	}
	return netlinkAddressOwned(rib, iface.Index, address)
}

func (o NetlinkVIPOwner) SetOwned(ctx context.Context, config Config, owned bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := o.Owned(ctx, config)
	if err != nil {
		return err
	}
	if current == owned {
		return nil
	}
	iface, err := net.InterfaceByName(config.VIPInterface.Name)
	if err != nil {
		if !owned {
			return nil
		}
		return fmt.Errorf("find endpoint interface %q: %w", config.VIPInterface.Name, err)
	}
	address, err := netip.ParsePrefix(config.Endpoint.VIP)
	if err != nil {
		return fmt.Errorf("parse endpoint address: %w", err)
	}
	action := "release"
	if owned {
		action = "acquire"
	}
	if err := setNetlinkAddress(ctx, iface.Index, address, owned); err != nil {
		return fmt.Errorf("%s local endpoint address: %w", action, err)
	}
	return nil
}

func netlinkAddressOwned(rib []byte, interfaceIndex int, address netip.Prefix) (bool, error) {
	messages, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return false, fmt.Errorf("parse route netlink addresses: %w", err)
	}
	want := address.Addr().AsSlice()
	for _, message := range messages {
		if message.Header.Type != syscall.RTM_NEWADDR || len(message.Data) < syscall.SizeofIfAddrmsg {
			continue
		}
		data := message.Data
		if int(binary.NativeEndian.Uint32(data[4:8])) != interfaceIndex || int(data[1]) != address.Bits() {
			continue
		}
		for attributes := data[syscall.SizeofIfAddrmsg:]; len(attributes) >= syscall.SizeofRtAttr; {
			length := int(binary.NativeEndian.Uint16(attributes[0:2]))
			if length < syscall.SizeofRtAttr || length > len(attributes) {
				return false, fmt.Errorf("parse route netlink address attribute")
			}
			attributeType := binary.NativeEndian.Uint16(attributes[2:4])
			value := attributes[syscall.SizeofRtAttr:length]
			if (attributeType == syscall.IFA_LOCAL || attributeType == syscall.IFA_ADDRESS) && net.IP(value).Equal(net.IP(want)) {
				return true, nil
			}
			aligned := (length + 3) &^ 3
			if aligned > len(attributes) {
				return false, fmt.Errorf("parse aligned route netlink address attribute")
			}
			attributes = attributes[aligned:]
		}
	}
	return false, nil
}

func setNetlinkAddress(ctx context.Context, interfaceIndex int, address netip.Prefix, owned bool) error {
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	timeout := 2 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout <= 0 {
		return context.DeadlineExceeded
	}
	timeval := syscall.NsecToTimeval(timeout.Nanoseconds())
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeval); err != nil {
		return err
	}
	request := marshalNetlinkAddressRequest(interfaceIndex, address, owned)
	if err := syscall.Sendto(fd, request, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	response := make([]byte, 4096)
	count, _, err := syscall.Recvfrom(fd, response, 0)
	if err != nil {
		return err
	}
	messages, err := syscall.ParseNetlinkMessage(response[:count])
	if err != nil {
		return err
	}
	for _, message := range messages {
		if message.Header.Type != syscall.NLMSG_ERROR || len(message.Data) < 4 {
			continue
		}
		code := int32(binary.NativeEndian.Uint32(message.Data[:4]))
		if code == 0 {
			return nil
		}
		return syscall.Errno(-code)
	}
	return fmt.Errorf("route netlink did not acknowledge address change")
}

func marshalNetlinkAddressRequest(interfaceIndex int, address netip.Prefix, owned bool) []byte {
	ip := address.Addr().AsSlice()
	attributeLength := syscall.SizeofRtAttr + len(ip)
	attributeSize := (attributeLength + 3) &^ 3
	attributeCount := 1
	if address.Addr().Is4() {
		attributeCount = 2
	}
	length := syscall.SizeofNlMsghdr + syscall.SizeofIfAddrmsg + attributeSize*attributeCount
	request := make([]byte, length)
	binary.NativeEndian.PutUint32(request[0:4], uint32(length))
	messageType := uint16(syscall.RTM_DELADDR)
	flags := uint16(syscall.NLM_F_REQUEST | syscall.NLM_F_ACK)
	if owned {
		messageType = syscall.RTM_NEWADDR
		flags |= syscall.NLM_F_CREATE | syscall.NLM_F_EXCL
	}
	binary.NativeEndian.PutUint16(request[4:6], messageType)
	binary.NativeEndian.PutUint16(request[6:8], flags)
	binary.NativeEndian.PutUint32(request[8:12], 1)
	body := request[syscall.SizeofNlMsghdr:]
	if address.Addr().Is4() {
		body[0] = syscall.AF_INET
	} else {
		body[0] = syscall.AF_INET6
	}
	body[1] = uint8(address.Bits())
	body[3] = syscall.RT_SCOPE_UNIVERSE
	binary.NativeEndian.PutUint32(body[4:8], uint32(interfaceIndex))
	attributes := body[syscall.SizeofIfAddrmsg:]
	writeNetlinkAddressAttribute(attributes, syscall.IFA_ADDRESS, ip)
	if attributeCount == 2 {
		writeNetlinkAddressAttribute(attributes[attributeSize:], syscall.IFA_LOCAL, ip)
	}
	return request
}

func writeNetlinkAddressAttribute(target []byte, attributeType uint16, value []byte) {
	binary.NativeEndian.PutUint16(target[0:2], uint16(syscall.SizeofRtAttr+len(value)))
	binary.NativeEndian.PutUint16(target[2:4], attributeType)
	copy(target[syscall.SizeofRtAttr:], value)
}

func parseProtocolStatus(output string, config Config) []PeerRuntimeStatus {
	known := map[string]PeerRuntimeStatus{}
	for _, peer := range config.FabricPeers {
		known[protocolName(peer)] = PeerRuntimeStatus{
			Name:          peer.Address,
			ASN:           peer.ASN,
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
		known["katl_exchange_"+safeSymbol(exchange.Name)+"_to_fabric"] = PeerRuntimeStatus{
			Name:          exchange.Name,
			Kind:          "route-exchange-export",
			AddressFamily: "ipv4",
			AdminState:    "unknown",
			SessionState:  "unknown",
		}
	}
	current := ""
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		topLevel := len(line) > 0 && line[0] != ' ' && line[0] != '\t'
		if topLevel && len(fields) >= 5 {
			if peer, ok := known[fields[0]]; ok {
				peer.AdminState = strings.ToLower(fields[3])
				peer.SessionState = peer.AdminState
				if len(fields) > 5 {
					peer.SessionState = strings.ToLower(strings.Join(fields[5:], "-"))
				}
				known[fields[0]] = peer
				current = fields[0]
				continue
			}
			current = ""
			continue
		}
		peer, ok := known[current]
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(trimmed, "Local address: "); ok {
			if values := strings.Fields(value); len(values) > 0 {
				peer.LocalAddress = values[0]
			}
		}
		var accepted, exported uint64
		if _, err := fmt.Sscanf(trimmed, "Routes: %d imported, %d exported", &accepted, &exported); err == nil {
			peer.AcceptedRoutes = accepted
			peer.ExportedRoutes = exported
		}
		known[current] = peer
	}
	out := make([]PeerRuntimeStatus, 0, len(known))
	for _, peer := range config.FabricPeers {
		out = append(out, known[protocolName(peer)])
	}
	for _, exchange := range config.RouteExchange {
		out = append(out, known["katl_exchange_"+safeSymbol(exchange.Name)])
		out = append(out, known["katl_exchange_"+safeSymbol(exchange.Name)+"_to_fabric"])
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
