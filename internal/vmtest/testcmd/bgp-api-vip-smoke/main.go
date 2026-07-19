package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
)

type proof struct {
	APIVersion             string                        `json:"apiVersion"`
	Kind                   string                        `json:"kind"`
	Mode                   string                        `json:"mode"`
	RenderedFiles          []string                      `json:"renderedFiles,omitempty"`
	Rejected               bool                          `json:"rejected,omitempty"`
	Rejection              string                        `json:"rejection,omitempty"`
	Statuses               []bgpapivip.Status            `json:"statuses,omitempty"`
	AdvertisementSequence  []bool                        `json:"advertisementSequence,omitempty"`
	ObservedRouteExports   []observedRouteChange         `json:"observedRouteExports,omitempty"`
	ObservedRouteWithdraws []observedRouteChange         `json:"observedRouteWithdraws,omitempty"`
	RouteTable             []string                      `json:"routeTable,omitempty"`
	PeerSummary            []bgpapivip.PeerRuntimeStatus `json:"peerSummary,omitempty"`
}

type observedRouteChange struct {
	Peer             string   `json:"peer"`
	ExportedPrefixes []string `json:"exportedPrefixes"`
}

type fakeBird struct {
	config         bgpapivip.Config
	advertisements []bool
	exports        []observedRouteChange
	withdrawals    []observedRouteChange
	routeTable     map[string]bool
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve-readyz":
			if err := serveReadyz(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		case "probe-readyz":
			if err := probeReadyz(os.Args[2:], os.Stdout); err != nil {
				log.Fatal(err)
			}
			return
		}
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func serveReadyz(args []string) error {
	flags := flag.NewFlagSet("serve-readyz", flag.ContinueOnError)
	listen := flags.String("listen", "", "listen address")
	cert := flags.String("cert", "", "TLS certificate")
	key := flags.String("key", "", "TLS private key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *listen == "" || *cert == "" || *key == "" {
		return errors.New("--listen, --cert, and --key are required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})
	server := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServeTLS(*cert, *key) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func probeReadyz(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("probe-readyz", flag.ContinueOnError)
	target := flags.String("url", "", "readyz URL")
	caPath := flags.String("ca", "", "CA certificate")
	serverName := flags.String("server-name", "", "TLS server name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	ca, err := os.ReadFile(*caPath)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("CA certificate is invalid")
	}
	client := http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: *serverName,
		}},
	}
	response, err := client.Get(*target)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		return fmt.Errorf("readyz returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	_, err = stdout.Write(body)
	return err
}

func run() error {
	configPath := strings.TrimSpace(os.Getenv("KATL_BGP_API_VIP_CONFIG"))
	if configPath == "" {
		return errors.New("KATL_BGP_API_VIP_CONFIG is required")
	}
	outputDir := strings.TrimSpace(os.Getenv("KATL_BGP_API_VIP_SMOKE_OUTPUT"))
	if outputDir == "" {
		outputDir = "/var/lib/katl/test-artifacts/bgp-api-vip-smoke"
	}
	mode := strings.TrimSpace(os.Getenv("KATL_BGP_API_VIP_SMOKE_MODE"))
	if mode == "" {
		mode = "control-plane"
	}
	object, err := readConfig(configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	switch mode {
	case "control-plane":
		return runControlPlane(outputDir, object.Spec)
	case "worker":
		return runWorker(outputDir, object.Spec)
	default:
		return fmt.Errorf("unsupported KATL_BGP_API_VIP_SMOKE_MODE %q", mode)
	}
}

func readConfig(path string) (bgpapivip.Object, error) {
	file, err := os.Open(path)
	if err != nil {
		return bgpapivip.Object{}, err
	}
	defer file.Close()
	return bgpapivip.Decode(file)
}

func runControlPlane(outputDir string, config bgpapivip.Config) error {
	plan, err := bgpapivip.RenderNativeEtcFiles(bgpapivip.RenderRequest{
		NodeRole: "control-plane",
		Config:   config,
	})
	if err != nil {
		return err
	}
	rendered := make([]string, 0, len(plan.Files))
	for _, file := range plan.Files {
		target := filepath.Join(outputDir, "rendered", strings.TrimPrefix(file.Path, "/"))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(file.Content), file.Mode); err != nil {
			return err
		}
		rendered = append(rendered, target)
	}
	bird := &fakeBird{config: plan.Config, routeTable: map[string]bool{}}
	health := &sequenceHealth{results: []bgpapivip.HealthResult{
		{Healthy: true, StatusCode: 200},
		{Healthy: true, StatusCode: 200},
		{Healthy: false, StatusCode: 503, Error: "readyz failed: Bearer vmtest-secret"},
		{Healthy: false, StatusCode: 503, Error: "readyz failed: Bearer vmtest-secret"},
		{Healthy: false, StatusCode: 503, Error: "readyz failed: Bearer vmtest-secret"},
	}}
	controller := bgpapivip.Controller{
		Config:            plan.Config,
		GenerationID:      "vmtest-generation-0",
		AppPayloadVersion: "bgp-api-vip-v0.1.0-katl.1",
		Bird:              bird,
		Health:            health,
		Interface:         bgpapivip.AlwaysReadyInterface{},
		Writer: bgpapivip.FileStatusWriter{
			LivePath:      filepath.Join(outputDir, "status-live.json"),
			OperationPath: filepath.Join(outputDir, "status-operation.json"),
		},
		Clock: func() time.Time {
			return time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
		},
	}
	var statuses []bgpapivip.Status
	for i := 0; i < 5; i++ {
		status, err := controller.RunOnce(context.Background())
		statuses = append(statuses, status)
		if err != nil && status.HealthState != bgpapivip.HealthUnhealthy {
			return fmt.Errorf("RunOnce(%d): %w", i, err)
		}
	}
	if got, want := bird.advertisements, []bool{false, true, false}; !reflect.DeepEqual(got, want) {
		return fmt.Errorf("advertisements = %#v, want %#v", got, want)
	}
	if statuses[1].AdvertisementState != bgpapivip.AdvertisementAdvertised || statuses[4].WithdrawReason != "local-health-failed" {
		return fmt.Errorf("unexpected advertisement statuses: %#v", statuses)
	}
	if len(bird.exports) != 1 || len(bird.exports[0].ExportedPrefixes) != 1 || bird.exports[0].ExportedPrefixes[0] != plan.Config.Endpoint.VIP {
		return fmt.Errorf("route exports = %#v, want only %s", bird.exports, plan.Config.Endpoint.VIP)
	}
	if len(bird.routeTable) != 0 {
		return fmt.Errorf("route table after withdrawal = %#v, want empty", bird.routeTable)
	}
	if strings.Contains(statuses[4].HealthFailure, "vmtest-secret") {
		return fmt.Errorf("health failure leaked secret: %q", statuses[4].HealthFailure)
	}
	return writeProof(filepath.Join(outputDir, "proof.json"), proof{
		APIVersion:             "vmtest.katl.dev/v1alpha1",
		Kind:                   "BGPAPIVIPSmokeProof",
		Mode:                   "control-plane",
		RenderedFiles:          rendered,
		Statuses:               statuses,
		AdvertisementSequence:  bird.advertisements,
		ObservedRouteExports:   bird.exports,
		ObservedRouteWithdraws: bird.withdrawals,
		RouteTable:             bird.routes(),
		PeerSummary:            statuses[len(statuses)-1].PeerSummary,
	})
}

func runWorker(outputDir string, config bgpapivip.Config) error {
	_, err := bgpapivip.RenderNativeEtcFiles(bgpapivip.RenderRequest{
		NodeRole: "worker",
		Config:   config,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot advertise BGP API VIP") {
		return fmt.Errorf("worker render error = %v, want role rejection", err)
	}
	return writeProof(filepath.Join(outputDir, "proof.json"), proof{
		APIVersion: "vmtest.katl.dev/v1alpha1",
		Kind:       "BGPAPIVIPSmokeProof",
		Mode:       "worker",
		Rejected:   true,
		Rejection:  err.Error(),
	})
}

func (b *fakeBird) Status(context.Context) (bgpapivip.BirdRuntimeStatus, error) {
	return bgpapivip.BirdRuntimeStatus{
		ProcessActive:      true,
		ControlSocketReady: true,
		ControlSocketPath:  "/run/katl/apps/bird/bird.ctl",
		ReadinessState:     "ready",
		Peers: []bgpapivip.PeerRuntimeStatus{{
			Name:           "dev-host",
			Kind:           "dev-host",
			AddressFamily:  b.config.Endpoint.AddressFamily,
			SessionState:   "established",
			AdminState:     "up",
			LocalAddress:   b.config.Routing.SourceAddress,
			LastTransition: "2026-06-19T12:00:00Z",
		}},
	}, nil
}

func (b *fakeBird) SetAdvertisement(_ context.Context, enabled bool) error {
	b.advertisements = append(b.advertisements, enabled)
	change := observedRouteChange{Peer: "dev-host"}
	for _, peer := range append(append([]bgpapivip.Peer{}, b.config.FabricPeers...), b.config.DevHostPeers...) {
		for _, prefix := range peer.AllowedExportPrefixes {
			if prefix == b.config.Endpoint.VIP {
				change.ExportedPrefixes = append(change.ExportedPrefixes, prefix)
			}
		}
	}
	if enabled {
		for _, prefix := range change.ExportedPrefixes {
			b.routeTable[prefix] = true
		}
		b.exports = append(b.exports, change)
		return nil
	}
	for prefix := range b.routeTable {
		delete(b.routeTable, prefix)
	}
	b.withdrawals = append(b.withdrawals, change)
	return nil
}

func (b *fakeBird) routes() []string {
	routes := make([]string, 0, len(b.routeTable))
	for prefix := range b.routeTable {
		routes = append(routes, prefix)
	}
	return routes
}

type sequenceHealth struct {
	results []bgpapivip.HealthResult
	next    int
}

func (h *sequenceHealth) Check(context.Context, bgpapivip.Health) bgpapivip.HealthResult {
	if h.next >= len(h.results) {
		return h.results[len(h.results)-1]
	}
	result := h.results[h.next]
	h.next++
	return result
}

func writeProof(path string, value proof) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
