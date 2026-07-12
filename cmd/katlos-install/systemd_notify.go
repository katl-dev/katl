package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

var notifySystemd = sendSystemdNotification

func reportInstallerProgress(stdout io.Writer, message string, ready bool) {
	message = strings.Join(strings.Fields(message), " ")
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

func sendSystemdNotification(payload string) error {
	socket := strings.TrimSpace(os.Getenv("NOTIFY_SOCKET"))
	if socket == "" {
		return nil
	}
	if strings.HasPrefix(socket, "@") {
		socket = "\x00" + strings.TrimPrefix(socket, "@")
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(payload)); err != nil {
		return err
	}
	return nil
}
