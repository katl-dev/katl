package apiproxy

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	config, err := Normalize(Config{
		TLSName: "api.home.arpa",
		Listeners: []Listener{
			{Address: "10.20.0.11:7445", Exposure: ExposureWorkstation},
			{Address: "127.0.0.1:7445", Exposure: ExposureNodeLocal},
		},
		Backends: []Backend{
			{Name: "cp-2", Address: "10.20.0.12:6443"},
			{Name: "cp-1", Address: "10.20.0.11:6443", Local: true},
		},
	})
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if config.CheckInterval != 2*time.Second || config.CheckTimeout != 2*time.Second {
		t.Fatalf("health timing = %s/%s", config.CheckInterval, config.CheckTimeout)
	}
	if config.Listeners[0].Address != "10.20.0.11:7445" || config.Backends[0].Name != "cp-1" {
		t.Fatalf("normalized config = %#v", config)
	}
	rendered, err := Render(config)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	var roundTrip Config
	if err := json.Unmarshal([]byte(rendered), &roundTrip); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, err := Normalize(roundTrip); err != nil {
		t.Fatalf("Normalize(roundTrip) error = %v", err)
	}
}

func TestUnmarshalAllowsDefaultHealthTiming(t *testing.T) {
	var config Config
	if err := json.Unmarshal([]byte(`{
		"tlsServerName":"api.home.arpa",
		"listeners":[{"address":"127.0.0.1:7445","exposure":"node-local"}],
		"backends":[{"name":"cp-1","address":"10.20.0.11:6443"}]
	}`), &config); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	config, err := Normalize(config)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if config.CheckInterval != DefaultInterval || config.CheckTimeout != DefaultTimeout {
		t.Fatalf("health timing = %s/%s", config.CheckInterval, config.CheckTimeout)
	}
}

func TestNormalizeRejectsUnsafeSurface(t *testing.T) {
	base := Config{
		TLSName:   "api.home.arpa",
		Listeners: []Listener{{Address: "127.0.0.1:7445", Exposure: ExposureNodeLocal}},
		Backends:  []Backend{{Name: "cp-1", Address: "10.20.0.11:6443"}},
	}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "wildcard listener", mutate: func(c *Config) {
			c.Listeners = append(c.Listeners, Listener{Address: "0.0.0.0:7445", Exposure: ExposureWorkstation})
		}, want: "literal unicast"},
		{name: "wrong listener port", mutate: func(c *Config) { c.Listeners[0].Address = "127.0.0.1:6443" }, want: "port must be 7445"},
		{name: "proxy backend", mutate: func(c *Config) { c.Backends[0].Address = "10.20.0.11:7445" }, want: "port must be 6443"},
		{name: "hostname backend", mutate: func(c *Config) { c.Backends[0].Address = "api.home.arpa:6443" }, want: "literal unicast"},
		{name: "missing local listener", mutate: func(c *Config) { c.Listeners = []Listener{{Address: "10.20.0.11:7445", Exposure: ExposureWorkstation}} }, want: "node-local access"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			config.Listeners = slices.Clone(base.Listeners)
			config.Backends = slices.Clone(base.Backends)
			test.mutate(&config)
			_, err := Normalize(config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Normalize() error = %v, want %q", err, test.want)
			}
		})
	}
}
