package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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
	var deferredPromotions []string
	if dispatcher == nil {
		executor := NewExecutor(root, store, agentServer.AgentStartID)
		executor.nodeBootHealth = agentServer.nodeBootHealth
		deferredPromotions, err = executor.recoverLivePromotions(ctx, currentBootID())
		if err != nil {
			return err
		}
		dispatcher = executor
	}
	if _, err := AuditStartup(store, timeNow(), deferredPromotions...); err != nil {
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
	err = agentServer.serveReloadable(ctx, network, address, notifyReady)
	return errors.Join(err, shutdownDispatcher(dispatcher))
}

// serveReloadable serves management and re-serves it on SIGHUP.
// katlc-agent.service is Type=notify-reload: systemd sends SIGHUP when an
// on-node command changed management state. The handler is registered before
// READY=1 so a reload never meets the default action, which ends the process.
func (s *Server) serveReloadable(ctx context.Context, network, address string, ready func() error) error {
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	defer signal.Stop(hangups)
	serveCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		for {
			select {
			case <-serveCtx.Done():
				return
			case <-hangups:
				s.management.reselect()
			}
		}
	}()
	return s.serveManagement(serveCtx, network, address, ready)
}

// serveManagement serves the agent API with the credentials the node's
// management state selects. It is the only owner of switching them: when a
// trial begins, is reverted or reaches its deadline, or systemd reloads the
// agent after an on-node change, it re-serves on the same address in this
// process, so a credential switch does not interrupt operations the agent
// runs.
func (s *Server) serveManagement(ctx context.Context, network, address string, ready func() error) error {
	clock := s.trialClock()
	for first := true; ; first = false {
		s.management.drain()
		// A SIGHUP drained here belongs to a reload this re-serve completes, so
		// the report that ends the re-serve is stamped after the drain.
		drained, err := monotonicUsec()
		if err != nil {
			return err
		}
		tlsConfig, trust, trial, err := managementServerTLS(s.Root, clock)
		if err != nil {
			return err
		}
		var remaining time.Duration
		if trial != nil {
			remaining = trialRemaining(*trial, clock)
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
		committed := s.management.serve(trial)
		agentapi.RegisterKatlcAgentServer(server, &managementListenerAPI{Server: s, trust: trust, committed: committed})
		errc := make(chan error, 1)
		go func() { errc <- server.Serve(listener) }()
		// The listener is bound, so connections made from here on are accepted.
		if first {
			if err := ready(); err != nil {
				server.Stop()
				return err
			}
		} else if err := notifyReloaded(drained); err != nil {
			log.Printf("report re-served management credentials: %v", err)
		}

		watchCtx, stopWatch := context.WithCancel(ctx)
		expired := make(chan struct{})
		if trial != nil {
			go func() {
				if s.awaitTrialDeadline(watchCtx, *trial, clock, remaining, committed) {
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
		case <-s.management.changes():
			log.Printf("management state changed; serving the credentials it selects")
		}
		stopWatch()
		if err := notifyReloading(); err != nil {
			log.Printf("report management credential reload: %v", err)
		}
		stopServer(server, managementReserveTimeout)
	}
}

// Each gRPC server keeps its own trust report while in-flight calls drain.
type managementListenerAPI struct {
	*Server
	trust     *agentapi.ManagementClientTrust
	committed <-chan struct{}
}

func (s *managementListenerAPI) GetNodeStatus(ctx context.Context, req *agentapi.GetNodeStatusRequest) (*agentapi.NodeStatus, error) {
	status, err := s.Server.GetNodeStatus(ctx, req)
	if err != nil {
		return nil, err
	}
	status.Management.ServedClientTrust = s.trust
	select {
	case <-s.committed:
		// Commit publishes the installed mode without replacing this listener.
		status.Management.ServedClientTrust = &agentapi.ManagementClientTrust{Authorities: s.trust.Authorities}
	default:
	}
	return status, nil
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
