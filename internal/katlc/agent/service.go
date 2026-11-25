package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/nodeidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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
	agentServer := NewServer(root, store)
	dispatcher := config.Dispatcher
	if dispatcher == nil {
		executor := NewExecutor(root, store, agentServer.AgentStartID)
		if err := executor.recoverLivePromotions(ctx, currentBootID()); err != nil {
			return err
		}
		dispatcher = executor
	}
	if _, err := AuditStartup(store, timeNow()); err != nil {
		return err
	}
	if err := recoverInterruptedConfigApplies(root, store, timeNow()); err != nil {
		// Keep management available so an operator can inspect and repair the
		// interrupted operation; its resource locks still block mutation.
		log.Printf("recover interrupted configuration apply: %v", err)
	}

	agentServer.Dispatcher = dispatcher
	maintenanceCtx, stopMaintenance := context.WithCancel(ctx)
	maintenanceDone := make(chan struct{})
	defer func() { stopMaintenance(); <-maintenanceDone }()
	go func() { defer close(maintenanceDone); agentServer.maintainGenerations(maintenanceCtx, 5*time.Second) }()
	err = agentServer.serveManagement(ctx, network, address, notifyReady)
	return errors.Join(err, shutdownDispatcher(dispatcher))
}

// serveManagement serves the agent API with the credentials the node's
// management state selects. When a served trial is reverted or reaches its
// deadline it serves the installed mode again on the same address, so the
// trial ends even if the systemd revert timer never runs.
func (s *Server) serveManagement(ctx context.Context, network, address string, ready func() error) error {
	clock := s.trialClock()
	for first := true; ; first = false {
		tlsConfig, trial, err := managementServerTLS(s.Root, clock)
		if err != nil {
			return err
		}
		listener, err := net.Listen(network, address)
		if err != nil {
			return err
		}
		// Rebind the address the first listener chose.
		address = listener.Addr().String()
		opts := []grpc.ServerOption{grpc.MaxRecvMsgSize(256 << 20), grpc.MaxSendMsgSize(256 << 20)}
		if tlsConfig != nil {
			opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfig)))
		}
		server := grpc.NewServer(opts...)
		agentapi.RegisterKatlcAgentServer(server, s)
		committed, reverted := s.servedTrial.serve(trial)
		errc := make(chan error, 1)
		go func() { errc <- server.Serve(listener) }()
		// The listener is bound, so connections made from here on are accepted.
		if first {
			if err := ready(); err != nil {
				server.Stop()
				return err
			}
		}

		watchCtx, stopWatch := context.WithCancel(ctx)
		expired := make(chan struct{})
		if trial != nil {
			go func() {
				if s.awaitTrialDeadline(watchCtx, *trial, clock, committed, reverted) {
					close(expired)
				}
			}()
		}
		select {
		case <-ctx.Done():
			stopWatch()
			server.GracefulStop()
			return ctx.Err()
		case err := <-errc:
			stopWatch()
			return err
		case <-expired:
			log.Printf("management trial %s reached its deadline; serving the installed management mode", trial.ID)
			// The revert timer may have reverted it already; the revert is
			// scoped to this trial and runs at most once.
			if _, _, err := nodeidentity.RevertManagementTrial(s.Root, trial.ID, managementTrialDeadlineReason, clock); err != nil {
				log.Printf("revert management trial %s: %v", trial.ID, err)
			}
		case <-reverted:
			log.Printf("management trial %s was reverted; serving the installed management mode", trial.ID)
		}
		stopWatch()
		stopServer(server, managementReserveTimeout)
	}
}

// managementReserveTimeout bounds how long in-flight calls on a listener that
// served an ended trial may finish before it is closed.
const managementReserveTimeout = 5 * time.Second

func stopServer(server *grpc.Server, timeout time.Duration) {
	stopped := make(chan struct{})
	go func() { server.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(timeout):
		server.Stop()
		<-stopped
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
