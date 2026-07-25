package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
)

var timeNow = func() time.Time { return time.Now().UTC() }

type ServeConfig struct {
	Root       string
	Listen     string
	Dispatcher Dispatcher
}

type dispatcherShutdown interface {
	Shutdown(context.Context) error
}

const dispatcherShutdownTimeout = 30 * time.Second

func Serve(ctx context.Context, config ServeConfig) error {
	root := strings.TrimSpace(config.Root)
	if root == "" {
		root = "/"
	}
	store, err := operation.NewStore(filepath.Join(root, "var/lib/katl/operations"))
	if err != nil {
		return err
	}
	listen := strings.TrimSpace(config.Listen)
	if listen == "" {
		listen = DefaultListen
	}
	network, address, err := parseListen(listen)
	if err != nil {
		return err
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := grpc.NewServer(
		grpc.MaxRecvMsgSize(256<<20),
		grpc.MaxSendMsgSize(256<<20),
	)
	agentServer := NewServer(root, store)
	if _, err := AuditStartup(store, timeNow()); err != nil {
		return err
	}
	dispatcher := config.Dispatcher
	if dispatcher == nil {
		dispatcher = NewExecutor(root, store, agentServer.AgentStartID)
	}
	agentServer.Dispatcher = dispatcher
	agentapi.RegisterKatlcAgentServer(server, agentServer)
	errc := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil {
			errc <- err
		}
		close(errc)
	}()
	select {
	case <-ctx.Done():
		server.GracefulStop()
		return errors.Join(ctx.Err(), shutdownDispatcher(dispatcher))
	case err := <-errc:
		return errors.Join(err, shutdownDispatcher(dispatcher))
	}
}

func shutdownDispatcher(dispatcher Dispatcher) error {
	shutdown, ok := dispatcher.(dispatcherShutdown)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), dispatcherShutdownTimeout)
	defer cancel()
	return shutdown.Shutdown(ctx)
}

func parseListen(value string) (string, string, error) {
	scheme, address, ok := strings.Cut(value, "://")
	if !ok {
		return "", "", fmt.Errorf("listen address must use scheme://address")
	}
	switch scheme {
	case "tcp":
		if strings.TrimSpace(address) == "" {
			return "", "", fmt.Errorf("tcp listen address is required")
		}
		return "tcp", address, nil
	default:
		return "", "", fmt.Errorf("unsupported listen scheme %q", scheme)
	}
}
