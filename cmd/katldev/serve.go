package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

func newWorldServeCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	port := 8080
	cmd := &cobra.Command{
		Use:   "serve DIR",
		Short: "Serve a directory to the world's VMs, such as a config bundle for katl.bundle.url",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			manager, err := loadInstallerManager(stdout, stderr)
			if err != nil {
				return err
			}
			world, err := manager.requireWorld()
			if err != nil {
				return err
			}
			address := net.JoinHostPort(world.World.Network.Gateway, strconv.Itoa(port))
			listener, err := net.Listen("tcp", address)
			if err != nil {
				return fmt.Errorf("listen on %s: %w; the world network is created by katldev world start", address, err)
			}
			fmt.Fprintf(stdout, "Serving %s at http://%s/ until interrupted.\n", args[0], address)
			return serveDirectory(ctx, listener, args[0])
		},
	}
	cmd.Flags().IntVar(&port, "port", port, "TCP port on the world network's host address")
	return cmd
}

func serveDirectory(ctx context.Context, listener net.Listener, dir string) error {
	server := &http.Server{Handler: http.FileServer(http.Dir(dir)), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
