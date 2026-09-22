package extensionrelease

import (
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/kernelmodule"
)

func TestReleaseSelection(t *testing.T) {
	target := Target{
		Version:          "2026.9.1",
		Architecture:     "x86_64",
		Flavour:          "standard",
		RuntimeInterface: "katl-runtime-1",
		Kernel: kernelmodule.Target{
			Release:       "6.12",
			RuntimeSHA256: strings.Repeat("a", 64),
		},
	}
	ref := "registry.example/drbd9@sha256:" + strings.Repeat("b", 64)
	release := Manifest{
		Target:     target,
		Extensions: map[string]string{"registry.example/drbd9": ref},
	}
	got, err := release.Resolve(target, "registry.example/drbd9")
	if err != nil || got != ref {
		t.Fatalf("selection = %q, %v", got, err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Target)
	}{
		{"release", func(t *Target) { t.Version = "2026.9.2" }},
		{"architecture", func(t *Target) { t.Architecture = "arm64" }},
		{"flavor", func(t *Target) { t.Flavour = "lts" }},
		{"build", func(t *Target) { t.Kernel.RuntimeSHA256 = strings.Repeat("c", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			other := target
			test.mutate(&other)
			if _, err := release.Resolve(other, "registry.example/drbd9"); err == nil {
				t.Fatal("accepted another target's manifest")
			}
		})
	}
	if _, err := release.Resolve(target, "registry.example/nvidia"); err == nil {
		t.Fatal("missing extension selected implicitly")
	}
	release.Extensions["registry.example/drbd9"] = "registry.example/drbd9:2026.9.1"
	if err := release.Validate(); err == nil {
		t.Fatal("accepted mutable release entry")
	}
}

func TestRepositorySelectors(t *testing.T) {
	for _, value := range []string{
		"ghcr.io/acme/extensions/drbd9",
		"registry.example:5000/team/driver",
		"localhost:5000/driver",
	} {
		if err := ValidateRepository(value); err != nil {
			t.Errorf("repository %q: %v", value, err)
		}
	}

	for _, value := range []string{
		"drbd9",
		"acme/drbd9",
		"https://registry.example/driver",
		"registry.example/driver:latest",
		"registry.example/driver@sha256:" + strings.Repeat("a", 64),
		" registry.example/driver",
		"registry.example/driver ",
	} {
		if err := ValidateRepository(value); err == nil {
			t.Errorf("accepted non-repository selector %q", value)
		}
	}
}

func TestRepositoryMapping(t *testing.T) {
	target := Target{
		Version:          "2026.9.1",
		Architecture:     "x86_64",
		Flavour:          "standard",
		RuntimeInterface: "katl-runtime-1",
		Kernel: kernelmodule.Target{
			Release:       "6.12",
			RuntimeSHA256: strings.Repeat("a", 64),
		},
	}
	first := "registry.example/team-a/driver@sha256:" + strings.Repeat("b", 64)
	second := "other.example/team-b/driver@sha256:" + strings.Repeat("c", 64)
	release := Manifest{
		Target: target,
		Extensions: map[string]string{
			"registry.example/team-a/driver": first,
			"other.example/team-b/driver":    second,
		},
	}
	got, err := release.Resolve(target, "other.example/team-b/driver")
	if err != nil || got != second {
		t.Fatalf("full repository selection = %q, %v", got, err)
	}
	if _, err := release.Resolve(target, "registry.example/team-b/driver"); err == nil {
		t.Fatal("resolved an unadvertised repository by basename or namespace")
	}

	release.Extensions["other.example/team-b/driver"] = first
	if err := release.Validate(); err == nil {
		t.Fatal("accepted a mapping to another repository")
	}
}
