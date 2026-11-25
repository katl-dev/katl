package operatorconsole

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"

	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

const (
	installerServiceName = "katlos-install.service"

	// StateInstallerFailed is shown when the installer service has stopped
	// without leaving a failed install status record.
	StateInstallerFailed = "installer-failed"

	// StatePreparingInstall is shown while an automatic install has reserved
	// the installer but has not begun its first step.
	StatePreparingInstall = "preparing-install"

	installerFailureHint = "fix the cause above, then reboot to restart the installer; Ctrl+Alt+F2 opens a console where journalctl -u katlos-install has the full log"
)

// InstallerService is the installer service's systemd state. Status is the
// STATUS text the installer last reported, which systemd keeps after exit.
type InstallerService struct {
	ActiveState string
	Result      string
	Status      string
}

func (s InstallerService) failed() bool {
	return s.ActiveState == "failed"
}

func readInstallerService(ctx context.Context) (InstallerService, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "systemctl", "show", installerServiceName,
		"--property=ActiveState,Result,StatusText").Output()
	if err != nil {
		return InstallerService{}, err
	}
	return parseInstallerService(output), nil
}

func parseInstallerService(output []byte) InstallerService {
	var service InstallerService
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			service.ActiveState = value
		case "Result":
			service.Result = value
		case "StatusText":
			service.Status = strings.TrimSpace(value)
		}
	}
	return service
}

func (c Collector) collectInstallerService(snapshot *Snapshot) {
	read := c.InstallerService
	if read == nil && c.Root == "" {
		read = readInstallerService
	}
	if read == nil {
		return
	}
	service, err := read(context.Background())
	if err != nil {
		return
	}
	if !service.failed() {
		snapshot.InstallerStatus = service.Status
		return
	}
	snapshot.InstallerStopped = true
	switch snapshot.State {
	case installstatus.StateFailedBeforeMutation, installstatus.StateFailedAfterMutation, installstatus.StateInstallRefused:
	default:
		snapshot.State = StateInstallerFailed
		snapshot.StatusStale = false
	}
	if snapshot.LastError == "" {
		snapshot.LastError = fallback(service.Status, "katlos-install exited unexpectedly ("+fallback(service.Result, "unknown")+")")
	}
	if snapshot.RetryHint == "" {
		snapshot.RetryHint = installerFailureHint
	}
}
