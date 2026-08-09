package firewall

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	TableName           = "katl_management"
	DefaultSnapshotPath = "/run/katl/management-interfaces"
)

type RunCommand func(context.Context, []string, []byte) ([]byte, error)

type Config struct {
	Port         uint16
	Interfaces   []net.Interface
	SnapshotPath string
	Run          RunCommand
}

// Ensure installs the node management ingress boundary once per boot. The
// first invocation snapshots interfaces that exist after host networking is
// online; a later service restart must not adopt workload interfaces.
func Ensure(ctx context.Context, config Config) error {
	if config.Port == 0 {
		return fmt.Errorf("management port is required")
	}
	run := config.Run
	if run == nil {
		run = runNFT
	}
	if _, err := run(ctx, []string{"list", "table", "inet", TableName}, nil); err == nil {
		return nil
	}
	interfaces := config.Interfaces
	snapshotPath := strings.TrimSpace(config.SnapshotPath)
	if snapshotPath == "" && config.Interfaces == nil {
		snapshotPath = DefaultSnapshotPath
	}
	if snapshotPath != "" {
		stored, err := readSnapshot(snapshotPath)
		if err == nil {
			interfaces = stored
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if interfaces == nil {
		var err error
		interfaces, err = net.Interfaces()
		if err != nil {
			return fmt.Errorf("list host network interfaces: %w", err)
		}
	}
	if snapshotPath != "" {
		if err := writeSnapshot(snapshotPath, interfaces); err != nil {
			return err
		}
	}
	rules, err := Render(config.Port, interfaces)
	if err != nil {
		return err
	}
	output, err := run(ctx, []string{"-f", "-"}, []byte(rules))
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("install management firewall: %w: %s", err, message)
		}
		return fmt.Errorf("install management firewall: %w", err)
	}
	return nil
}

func readSnapshot(path string) ([]net.Interface, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var interfaces []net.Interface
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.ContainsAny(name, "\x00\r\n") {
			return nil, fmt.Errorf("management interface snapshot %s is invalid", path)
		}
		interfaces = append(interfaces, net.Interface{Name: name})
	}
	if len(interfaces) == 0 {
		return nil, fmt.Errorf("management interface snapshot %s is empty", path)
	}
	return interfaces, nil
}

func writeSnapshot(path string, interfaces []net.Interface) error {
	names := make([]string, 0, len(interfaces))
	for _, networkInterface := range interfaces {
		name := strings.TrimSpace(networkInterface.Name)
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("no host network interfaces are available for management access")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create management interface snapshot directory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".management-interfaces-*")
	if err != nil {
		return fmt.Errorf("create management interface snapshot: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(strings.Join(names, "\n") + "\n"); err != nil {
		temporary.Close()
		return fmt.Errorf("write management interface snapshot: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish management interface snapshot: %w", err)
	}
	return nil
}

func Render(port uint16, interfaces []net.Interface) (string, error) {
	if port == 0 {
		return "", fmt.Errorf("management port is required")
	}
	names := make([]string, 0, len(interfaces))
	seen := make(map[string]struct{}, len(interfaces))
	for _, networkInterface := range interfaces {
		name := strings.TrimSpace(networkInterface.Name)
		if name == "" || strings.ContainsRune(name, '\x00') {
			return "", fmt.Errorf("host network interface name is invalid")
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no host network interfaces are available for management access")
	}
	sort.Strings(names)
	quoted := make([]string, len(names))
	for index, name := range names {
		quoted[index] = strconv.Quote(name)
	}
	return fmt.Sprintf(`table inet %s {
	set host_interfaces {
		type ifname
		elements = { %s }
	}

	chain input {
		type filter hook input priority -200; policy accept;
		tcp dport %d iifname != @host_interfaces drop
	}
}
`, TableName, strings.Join(quoted, ", "), port), nil
}

func runNFT(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, "nft", args...)
	command.Stdin = bytes.NewReader(stdin)
	return command.CombinedOutput()
}
