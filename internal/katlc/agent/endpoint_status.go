package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func controlPlaneEndpointStatus(root string) (*agentapi.ControlPlaneEndpointStatus, error) {
	configFile, err := os.Open(rootedRuntimePath(root, bgpapivip.ConfigPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open managed endpoint configuration: %w", err)
	}
	object, decodeErr := bgpapivip.Decode(configFile)
	closeErr := configFile.Close()
	if decodeErr != nil {
		return nil, decodeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	config, err := bgpapivip.Normalize(object.Spec)
	if err != nil {
		return nil, err
	}

	report := endpointStatusFrom(config, nil)
	statusFile, err := os.Open(rootedRuntimePath(root, bgpapivip.LiveStatusPath))
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		report.State = "failed"
		report.FailureReason = "endpoint status unavailable"
		return report, nil
	}
	live, decodeErr := bgpapivip.DecodeStatus(statusFile)
	closeErr = statusFile.Close()
	if decodeErr != nil || closeErr != nil {
		report.State = "failed"
		report.FailureReason = "endpoint status unavailable"
		return report, nil
	}
	return endpointStatusFrom(config, &live), nil
}

func endpointStatusFrom(config bgpapivip.Config, live *bgpapivip.Status) *agentapi.ControlPlaneEndpointStatus {
	report := &agentapi.ControlPlaneEndpointStatus{
		Endpoint: net.JoinHostPort(config.Endpoint.Host, fmt.Sprint(config.Endpoint.Port)),
		Vip:      config.Endpoint.VIP,
		State:    "starting",
		RouterId: config.Routing.RouterID,
	}
	if config.Advertisement.Enabled == nil || !*config.Advertisement.Enabled {
		report.State = "disabled"
	}

	fabric := make(map[string]bgpapivip.PeerRuntimeStatus)
	exchanges := make(map[string]bgpapivip.PeerRuntimeStatus)
	exports := make(map[string]bgpapivip.PeerRuntimeStatus)
	if live != nil {
		report.LocalApiReady = live.HealthState == bgpapivip.HealthHealthy
		report.RouteOriginated = live.AdvertisementState == bgpapivip.AdvertisementAdvertised
		report.LocalVipOwned = live.LocalVIPOwned
		report.LastTransitionTime = firstNonEmpty(live.LastAdvertisementTransition, live.LastHealthTransition, live.UpdatedAt)
		report.FailureReason = strings.TrimSpace(live.FailureReason)
		for _, peer := range live.PeerSummary {
			switch peer.Kind {
			case "fabric":
				fabric[peer.Name] = peer
			case "route-exchange":
				exchanges[peer.Name] = peer
			case "route-exchange-export":
				exports[peer.Name] = peer
			}
			if report.SelectedSourceAddress == "" && peer.LocalAddress != "" {
				report.SelectedSourceAddress = peer.LocalAddress
			}
		}
		report.State = endpointProductState(config, *live, fabric)
	}
	if report.SelectedSourceAddress == "" {
		report.SelectedSourceAddress = config.Routing.SourceAddress
	}
	if report.RouterId == "" {
		report.RouterId = report.SelectedSourceAddress
	}

	for _, peer := range config.FabricPeers {
		runtime := fabric[peer.Address]
		report.Peers = append(report.Peers, &agentapi.ControlPlaneEndpointPeerStatus{
			Address:       peer.Address,
			Asn:           peer.ASN,
			State:         defaultProductState(runtime.SessionState),
			RouteExported: runtime.ExportedRoutes > 0,
		})
	}
	for _, exchange := range config.RouteExchange {
		runtime := exchanges[exchange.Name]
		export := exports[exchange.Name]
		report.RouteExchange = append(report.RouteExchange, &agentapi.ControlPlaneEndpointRouteExchangeStatus{
			Name:           exchange.Name,
			ListenAddress:  "127.0.0.1",
			ListenPort:     uint32(exchange.ListenPort),
			PeerAsn:        exchange.PeerASN,
			State:          defaultProductState(runtime.SessionState),
			AcceptedRoutes: runtime.AcceptedRoutes,
			ExportedRoutes: export.ExportedRoutes,
		})
	}
	return report
}

func endpointProductState(config bgpapivip.Config, live bgpapivip.Status, peers map[string]bgpapivip.PeerRuntimeStatus) string {
	if config.Advertisement.Enabled == nil || !*config.Advertisement.Enabled {
		return "disabled"
	}
	if live.RecoveryRequired || strings.TrimSpace(live.FailureReason) != "" {
		return "failed"
	}
	if !live.VIPInterfaceReady ||
		(config.Routing.Mode != "vip-only" && (!live.BirdProcessActive || !live.BirdControlSocketReady)) {
		return "waiting-for-network"
	}
	if strings.Contains(strings.ToLower(live.HealthFailure), "kubeadm api ca") {
		return "waiting-for-kubeadm-ca"
	}
	if live.HealthState != bgpapivip.HealthHealthy {
		return "waiting-for-apiserver"
	}
	if len(config.FabricPeers) > 0 {
		established := false
		for _, peer := range peers {
			if strings.EqualFold(peer.SessionState, "established") {
				established = true
				break
			}
		}
		if !established {
			return "waiting-for-peer"
		}
	}
	if live.AdvertisementState == bgpapivip.AdvertisementAdvertised && live.LocalVIPOwnedReported && !live.LocalVIPOwned {
		return "failed"
	}
	if live.AdvertisementState == bgpapivip.AdvertisementAdvertised {
		return "advertised"
	}
	if live.LocalVIPOwned {
		return "waiting-for-route"
	}
	return "withdrawn"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func defaultProductState(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "unknown"
}
