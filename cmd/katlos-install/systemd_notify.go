package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/katl-dev/katl/internal/systemdunit"
)

var notifySystemd = systemdunit.Notify

func reportInstallerProgress(stdout io.Writer, message string, ready bool) {
	message = compactMessage(message)
	if message == "" {
		return
	}
	if stdout == nil {
		stdout = io.Discard
	}
	fmt.Fprintf(stdout, "katlos-install progress: %s\n", message)
	payload := "STATUS=" + message
	if ready {
		payload = "READY=1\n" + payload
	}
	if err := notifySystemd(payload); err != nil {
		fmt.Fprintf(stdout, "katlos-install progress: unable to update systemd status: %v\n", err)
	}
}

func compactMessage(message string) string {
	return strings.Join(strings.Fields(message), " ")
}

// reportInstallerFailure leaves the failure in the unit's STATUS, which
// systemd retains after the service exits, so the operator console can show
// why the installer stopped.
func reportInstallerFailure(err error) {
	message := compactMessage(err.Error())
	if message == "" {
		return
	}
	_ = notifySystemd("STATUS=installer stopped: " + message)
}
