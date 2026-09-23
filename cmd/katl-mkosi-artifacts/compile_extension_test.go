package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestNVIDIAReleaseInventory(t *testing.T) {
	repo := filepath.Clean(filepath.Join("..", ".."))
	recipe, err := readKernelRecipe(repo, "nvidia")
	if err != nil {
		t.Fatal(err)
	}
	if recipe.Repository != "ghcr.io/katl-dev/katl/extensions/nvidia" || len(recipe.Sources) != 2 {
		t.Fatalf("NVIDIA release recipe = %#v", recipe)
	}
	var inventory bytes.Buffer
	if err := runExtensionInventory(nil, &inventory, config{RepoRoot: repo}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inventory.String(), `"nvidia"`) {
		t.Fatalf("NVIDIA is absent from the release inventory: %s", inventory.String())
	}
}

func TestKernelSourceFile(t *testing.T) {
	for _, scenario := range []struct {
		url, want string
	}{
		{"https://example.invalid/source.tar.gz", strings.Repeat("a", 64) + ".tar.gz"},
		{"https://example.invalid/NVIDIA-Linux.run", strings.Repeat("a", 64) + ".run"},
	} {
		file, err := kernelSourceFile(kernelSource{URL: scenario.url, SHA256: strings.Repeat("a", 64)})
		if err != nil || file != scenario.want {
			t.Fatalf("source file for %s = %q, %v", scenario.url, file, err)
		}
	}
}
