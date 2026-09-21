package configapply

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
)

func TestRetentionConfiguration(t *testing.T) {
	count := 2
	request := trustedBundleRequest(t.TempDir(), TrustedBundleRequest{ApplyMode: generation.ApplyModeAuto, NodeOverrides: map[string]NodeOverlay{"cp-1": {GenerationRetention: &generation.Retention{KeepLast: &count, MaxAge: "7d"}}}})
	result, err := PlanTrustedBundle(request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan.Decision.AcceptedMode != generation.ApplyModeLive || !containsDomain(result.Plan.Decision.ChangedDomains, DomainGenerationRetention) {
		t.Fatalf("decision: %+v", result.Plan.Decision)
	}
	keep, age, err := result.Manifest.Node.GenerationRetention.Limits()
	if err != nil || keep != 2 || age != 7*24*time.Hour {
		t.Fatalf("policy: %d %s %v", keep, age, err)
	}

	request.CurrentManifest = result.Manifest
	if _, _, _, err := mergeRuntimeConfig(request); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("repeat changed configuration: %v", err)
	}
	data, err := RenderNodeConfigurationChange(RenderNodeRequest{NodeName: "cp-1", Manifest: result.Manifest, SourceID: "lab", DesiredVersion: "3"})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeNodeConfigurationChange(strings.NewReader(string(data)), TrustedBundleRequest{NodeName: "cp-1"})
	if err != nil {
		t.Fatal(err)
	}
	keep, age, err = decoded.NodeOverrides["cp-1"].GenerationRetention.Limits()
	if err != nil || keep != 2 || age != 7*24*time.Hour {
		t.Fatalf("rendered policy: %d %s %v", keep, age, err)
	}
}
