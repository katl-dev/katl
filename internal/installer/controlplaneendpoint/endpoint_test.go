package controlplaneendpoint

import (
	"strings"
	"testing"
)

func TestNormalizeExternalEndpoint(t *testing.T) {
	plan, err := Normalize(Config{Host: "api.home.example"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Endpoint != "api.home.example:6443" || plan.Config.Advertisement != nil {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestNormalizeManagedEndpoint(t *testing.T) {
	plan, err := Normalize(managedConfig())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Endpoint != "api.home.example:6443" || plan.VIPPrefix != "10.40.0.10/32" {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestNormalizeAcceptsMatchingIPLiteralHost(t *testing.T) {
	config := managedConfig()
	config.Host = config.Advertisement.VIP
	plan, err := Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Endpoint != "10.40.0.10:6443" {
		t.Fatalf("endpoint = %q", plan.Endpoint)
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
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := managedConfig()
			test.mutate(&config)
			_, err := Normalize(config)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Normalize() error = %v, want %q", err, test.want)
			}
		})
	}
}

func managedConfig() Config {
	return Config{Host: "api.home.example", Advertisement: &Advertisement{VIP: "10.40.0.10"}}
}
