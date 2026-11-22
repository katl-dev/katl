package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// libvirtGuestsConfig is how the host treats running VMs when it shuts down.
type libvirtGuestsConfig struct {
	Loaded     bool
	Enabled    bool
	OnShutdown string
}

// warnHostShutdown prints one line when host shutdown would suspend or kill
// the world's VMs. A resumed Katl node's clock jumps past DHCP leases and
// certificate checks, so worlds should shut down with the host. Failure to
// read the configuration is not reported.
func (manager installerManager) warnHostShutdown() {
	read := manager.guestsConfig
	if read == nil {
		read = readLibvirtGuestsConfig
	}
	config, err := read()
	if err != nil {
		return
	}
	if warning := libvirtGuestsWarning(config); warning != "" {
		fmt.Fprintln(manager.stderr, "katldev world: warning:", warning)
	}
}

func libvirtGuestsWarning(config libvirtGuestsConfig) string {
	switch {
	case !config.Loaded || !config.Enabled:
		return "libvirt-guests.service is not enabled, so host shutdown cuts power to world VMs; enable it with ON_SHUTDOWN=shutdown"
	case config.OnShutdown != "shutdown":
		return fmt.Sprintf("libvirt-guests.service uses ON_SHUTDOWN=%s, and resumed nodes jump their clocks past leases and certificates; set ON_SHUTDOWN=shutdown", config.OnShutdown)
	default:
		return ""
	}
}

func readLibvirtGuestsConfig() (libvirtGuestsConfig, error) {
	output, err := exec.Command("systemctl", "show", "libvirt-guests.service",
		"-p", "LoadState", "-p", "UnitFileState", "-p", "Environment", "-p", "EnvironmentFiles").Output()
	if err != nil {
		return libvirtGuestsConfig{}, err
	}
	return parseLibvirtGuestsConfig(output, os.ReadFile), nil
}

// parseLibvirtGuestsConfig applies systemd's precedence: EnvironmentFile=
// values override Environment=. libvirt-guests suspends when ON_SHUTDOWN is
// unset.
func parseLibvirtGuestsConfig(show []byte, readFile func(string) ([]byte, error)) libvirtGuestsConfig {
	config := libvirtGuestsConfig{OnShutdown: "suspend"}
	var environment, fromFiles string
	for _, line := range strings.Split(string(show), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "LoadState":
			config.Loaded = value == "loaded"
		case "UnitFileState":
			config.Enabled = value == "enabled" || value == "enabled-runtime"
		case "Environment":
			for _, assignment := range splitQuoted(value) {
				if name, setting, ok := strings.Cut(assignment, "="); ok && name == "ON_SHUTDOWN" {
					environment = setting
				}
			}
		case "EnvironmentFiles":
			fields := strings.Fields(value)
			if len(fields) == 0 {
				continue
			}
			data, err := readFile(strings.TrimPrefix(fields[0], "-"))
			if err != nil {
				continue
			}
			scanner := bufio.NewScanner(bytes.NewReader(data))
			for scanner.Scan() {
				name, setting, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
				if ok && strings.TrimSpace(name) == "ON_SHUTDOWN" {
					fromFiles = strings.Trim(strings.TrimSpace(setting), `"'`)
				}
			}
		}
	}
	if value := firstNonEmpty(fromFiles, environment); value != "" {
		config.OnShutdown = value
	}
	return config
}

func splitQuoted(value string) []string {
	var fields []string
	var current strings.Builder
	quoted := false
	for _, r := range value {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ' ' && !quoted:
			if current.Len() > 0 {
				fields = append(fields, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		fields = append(fields, current.String())
	}
	return fields
}
