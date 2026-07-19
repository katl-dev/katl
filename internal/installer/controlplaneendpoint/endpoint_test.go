package controlplaneendpoint

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeExternalEndpoint(t *testing.T) {
	plan, err := Normalize(Config{Host: "api.home.example"})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if plan.Endpoint != "api.home.example:6443" || plan.Config.Port != 6443 || plan.Config.Advertisement != nil {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestNormalizeManagedEndpoint(t *testing.T) {
	length := 32
	plan, err := Normalize(Config{
		Host: "api.home.example",
		Advertisement: &Advertisement{
			VIP: "10.40.0.10",
			BGP: &BGP{
				LocalASN: 64512,
				Peers: []Peer{
					{Address: "10.0.0.2", ASN: 64500},
					{Address: "10.0.0.1", ASN: 64512},
				},
				RouteExchange: []RouteExchange{{
					Name: "cilium",
					ExportToFabric: []PrefixEnvelope{
						{CIDR: "10.50.0.1/16", PrefixLength: &length},
						{CIDR: "10.50.0.0/16", PrefixLength: &length},
					},
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if plan.Endpoint != "api.home.example:6443" || plan.VIPPrefix != "10.40.0.10/32" {
		t.Fatalf("plan = %#v", plan)
	}
	bgp := plan.Config.Advertisement.BGP
	if got := bgp.Peers; !reflect.DeepEqual(got, []Peer{{Address: "10.0.0.1", ASN: 64512}, {Address: "10.0.0.2", ASN: 64500}}) {
		t.Fatalf("peers = %#v", got)
	}
	exchange := bgp.RouteExchange[0]
	if exchange.ListenPort != 179 || exchange.PeerASN != 64512 || len(exchange.ExportToFabric) != 1 || exchange.ExportToFabric[0].CIDR != "10.50.0.0/16" {
		t.Fatalf("route exchange = %#v", exchange)
	}
}

func TestNormalizeWarnsWhenRouteExchangeIncludesVIP(t *testing.T) {
	plan, err := Normalize(managedConfig())
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "route-exchange-includes-api-vip" {
		t.Fatalf("warnings = %#v", plan.Warnings)
	}
}

func TestNormalizeRejectsInvalidEndpointIntent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "empty host", mutate: func(c *Config) { c.Host = "" }, want: "host is required"},
		{name: "URL host", mutate: func(c *Config) { c.Host = "https://api.example" }, want: "not a URL"},
		{name: "bad port", mutate: func(c *Config) { c.Port = 70000 }, want: "between 1 and 65535"},
		{name: "CIDR VIP", mutate: func(c *Config) { c.Advertisement.VIP = "10.40.0.10/32" }, want: "bare IPv4"},
		{name: "loopback VIP", mutate: func(c *Config) { c.Advertisement.VIP = "127.0.0.1" }, want: "not a usable routed address"},
		{name: "IP host mismatch", mutate: func(c *Config) { c.Host = "10.40.0.11" }, want: "must equal"},
		{name: "missing BGP", mutate: func(c *Config) { c.Advertisement.BGP = nil }, want: "bgp is required"},
		{name: "reserved local ASN", mutate: func(c *Config) { c.Advertisement.BGP.LocalASN = 65535 }, want: "reserved"},
		{name: "empty peers", mutate: func(c *Config) { c.Advertisement.BGP.Peers = nil }, want: "must not be empty"},
		{name: "peer hostname", mutate: func(c *Config) { c.Advertisement.BGP.Peers[0].Address = "router.example" }, want: "usable IPv4"},
		{name: "duplicate peer", mutate: func(c *Config) {
			c.Advertisement.BGP.Peers = append(c.Advertisement.BGP.Peers, c.Advertisement.BGP.Peers[0])
		}, want: "duplicates another peer"},
		{name: "invalid exchange name", mutate: func(c *Config) { c.Advertisement.BGP.RouteExchange[0].Name = "Cilium_Routes" }, want: "DNS-label-style"},
		{name: "multiple default ports", mutate: func(c *Config) {
			c.Advertisement.BGP.RouteExchange = append(c.Advertisement.BGP.RouteExchange, RouteExchange{Name: "other"})
		}, want: "listenPort is required"},
		{name: "impossible prefix length", mutate: func(c *Config) {
			value := 8
			c.Advertisement.BGP.RouteExchange[0].ExportToFabric[0].PrefixLength = &value
		}, want: "between 24 and 32"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := managedConfig()
			tt.mutate(&config)
			_, err := Normalize(config)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Normalize() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func managedConfig() Config {
	length := 32
	return Config{
		Host: "api.home.example",
		Advertisement: &Advertisement{
			VIP: "10.40.0.10",
			BGP: &BGP{
				LocalASN: 64512,
				Peers:    []Peer{{Address: "10.0.0.1", ASN: 64500}},
				RouteExchange: []RouteExchange{{
					Name:           "cilium",
					ExportToFabric: []PrefixEnvelope{{CIDR: "10.40.0.0/24", PrefixLength: &length}},
				}},
			},
		},
	}
}
