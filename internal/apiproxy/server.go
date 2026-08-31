package apiproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const StatusKind = "APIProxyStatus"

type Status struct {
	APIVersion string                  `json:"apiVersion"`
	Kind       string                  `json:"kind"`
	UpdatedAt  time.Time               `json:"updatedAt"`
	Listeners  []Listener              `json:"listeners"`
	Backends   []BackendStatus         `json:"backends"`
	Canonical  CanonicalEndpointStatus `json:"canonicalEndpoint"`
}

type CanonicalEndpointStatus struct {
	Endpoint    string    `json:"endpoint"`
	State       string    `json:"state"`
	Reason      string    `json:"reason,omitempty"`
	LastChecked time.Time `json:"lastChecked,omitempty"`
}

type Server struct {
	Config     Config
	StatusPath string
	Logf       func(string, ...any)

	pool      *pool
	statusMu  sync.Mutex
	canonical CanonicalEndpointStatus
}

func (s *Server) Run(ctx context.Context) error {
	config, err := Normalize(s.Config)
	if err != nil {
		return err
	}
	s.Config = config
	if s.StatusPath == "" {
		s.StatusPath = StatusPath
	}
	s.pool = newPool(config.Backends)
	s.canonical = CanonicalEndpointStatus{Endpoint: config.CanonicalEndpoint, State: "not-checked"}
	_ = os.Remove(s.StatusPath)

	listeners := make([]net.Listener, 0, len(config.Listeners))
	for _, configured := range config.Listeners {
		listener, err := net.Listen("tcp", configured.Address)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return fmt.Errorf("listen on %s: %w", configured.Address, err)
		}
		listeners = append(listeners, listener)
	}
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	if err := s.writeStatus(); err != nil {
		return err
	}

	errCh := make(chan error, len(listeners))
	for _, listener := range listeners {
		go s.accept(ctx, listener, errCh)
	}
	for _, backend := range config.Backends {
		go s.monitor(ctx, backend)
	}
	go s.monitorCanonical(ctx)
	go func() {
		<-ctx.Done()
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) accept(ctx context.Context, listener net.Listener, errCh chan<- error) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case errCh <- fmt.Errorf("accept on %s: %w", listener.Addr(), err):
			default:
			}
			return
		}
		go s.serveConnection(ctx, conn)
	}
}

func (s *Server) serveConnection(ctx context.Context, client net.Conn) {
	defer client.Close()
	excluded := map[string]struct{}{}
	for {
		backend, err := s.pool.selectBackend(excluded)
		if err != nil {
			s.logf("rejecting API connection from %s: %v", client.RemoteAddr(), err)
			return
		}
		upstream, err := (&net.Dialer{Timeout: s.Config.CheckTimeout}).DialContext(ctx, "tcp", backend.Address)
		if err != nil {
			s.pool.update(backend.Name, false, err.Error(), time.Now().UTC())
			_ = s.writeStatus()
			excluded[backend.Name] = struct{}{}
			continue
		}
		s.forward(client, upstream)
		return
	}
}

func (s *Server) forward(client, upstream net.Conn) {
	defer upstream.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go copyConnection(&wg, upstream, client)
	go copyConnection(&wg, client, upstream)
	wg.Wait()
}

func copyConnection(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if closer, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = closer.CloseWrite()
	}
}

func (s *Server) monitor(ctx context.Context, backend Backend) {
	ticker := time.NewTicker(s.Config.CheckInterval)
	defer ticker.Stop()
	for {
		err := s.checkReady(ctx, backend)
		reason := "ready"
		if err != nil {
			reason = err.Error()
		}
		s.pool.update(backend.Name, err == nil, reason, time.Now().UTC())
		if statusErr := s.writeStatus(); statusErr != nil {
			s.logf("write API proxy status: %v", statusErr)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) checkReady(ctx context.Context, backend Backend) error {
	return s.checkReadyAddress(ctx, backend.Address)
}

func (s *Server) checkReadyAddress(ctx context.Context, address string) error {
	ca, err := os.ReadFile(s.Config.CAFile)
	if err != nil {
		return fmt.Errorf("read Kubernetes CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("Kubernetes CA contains no certificate")
	}
	checkCtx, cancel := context.WithTimeout(ctx, s.Config.CheckTimeout)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(checkCtx, "tcp", address)
	if err != nil {
		return err
	}
	defer raw.Close()
	tlsConn := tls.Client(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: s.Config.TLSName,
	})
	if err := tlsConn.HandshakeContext(checkCtx); err != nil {
		return fmt.Errorf("TLS handshake: %w", err)
	}
	request, err := http.NewRequestWithContext(checkCtx, http.MethodGet, "https://"+s.Config.TLSName+"/readyz", nil)
	if err != nil {
		return err
	}
	request.Close = true
	if err := request.Write(tlsConn); err != nil {
		return fmt.Errorf("write readiness request: %w", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(tlsConn), request)
	if err != nil {
		return fmt.Errorf("read readiness response: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz returned %s", response.Status)
	}
	return nil
}

func (s *Server) monitorCanonical(ctx context.Context) {
	ticker := time.NewTicker(s.Config.CheckInterval)
	defer ticker.Stop()
	for {
		err := s.checkReadyAddress(ctx, s.Config.CanonicalEndpoint)
		state := "reachable"
		reason := ""
		if err != nil {
			state = "unreachable"
			reason = err.Error()
		}
		s.statusMu.Lock()
		s.canonical = CanonicalEndpointStatus{
			Endpoint:    s.Config.CanonicalEndpoint,
			State:       state,
			Reason:      reason,
			LastChecked: time.Now().UTC(),
		}
		s.statusMu.Unlock()
		if statusErr := s.writeStatus(); statusErr != nil {
			s.logf("write API proxy status: %v", statusErr)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) writeStatus() error {
	if s.StatusPath == "" || s.pool == nil {
		return nil
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	status := Status{
		APIVersion: APIVersion,
		Kind:       StatusKind,
		UpdatedAt:  time.Now().UTC(),
		Listeners:  slices.Clone(s.Config.Listeners),
		Backends:   s.pool.snapshot(),
		Canonical:  s.canonical,
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal API proxy status: %w", err)
	}
	dir := filepath.Dir(s.StatusPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create API proxy status directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".status-*")
	if err != nil {
		return fmt.Errorf("create API proxy status: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.StatusPath); err != nil {
		return fmt.Errorf("install API proxy status: %w", err)
	}
	return nil
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}
