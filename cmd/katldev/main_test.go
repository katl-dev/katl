package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/artifact"
	"github.com/katl-dev/katl/internal/installer/katlosimage"
)

func TestRootAndInstallerCommandsShowHelpWithoutArguments(t *testing.T) {
	for _, test := range []struct {
		args []string
		want []string
	}{
		{want: []string{"build", "installer"}},
		{args: []string{"build"}, want: []string{"iso", "kubernetes", "upgrade"}},
		{args: []string{"installer"}, want: []string{"start", "reset", "status", "console", "stop"}},
	} {
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), test.args, &stdout, &stderr); err != nil {
			t.Fatalf("run(%v) error = %v, stderr=%s", test.args, err, stderr.String())
		}
		help := stdout.String()
		for _, want := range test.want {
			if !strings.Contains(help, want) {
				t.Fatalf("run(%v) help missing %q:\n%s", test.args, want, help)
			}
		}
	}
}

func TestBuildInstallerISOComposesSupportedPipeline(t *testing.T) {
	repo := t.TempDir()
	iso := filepath.Join(repo, "_build", "mkosi", "katl-installer.iso")
	contents := []byte("current checkout installer ISO")
	type call struct {
		dir  string
		name string
		args []string
		env  []string
	}
	var calls []call
	runner := func(_ context.Context, dir, name string, args, environment []string, _, _ io.Writer) error {
		calls = append(calls, call{dir: dir, name: name, args: append([]string(nil), args...), env: append([]string(nil), environment...)})
		if filepath.Base(name) == "mkosi" {
			if err := os.MkdirAll(filepath.Dir(iso), 0o755); err != nil {
				return err
			}
			for path, data := range map[string][]byte{iso: contents, iso + ".json": []byte("{}\n"), iso + ".sha256": []byte("checksum\n")} {
				if err := os.WriteFile(path, data, 0o644); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	artifact, err := buildInstallerISO(context.Background(), repo, &stderr, runner)
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []call{
		{dir: repo, name: filepath.Join(repo, "scripts", "mkosi"), args: []string{"build-installer-iso"}},
		{dir: repo, name: filepath.Join(repo, "scripts", "check-installer-iso"), args: []string{iso}},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("build calls = %#v, want %#v", calls, wantCalls)
	}
	digest := sha256.Sum256(contents)
	if artifact.Path != iso || artifact.Metadata != iso+".json" || artifact.Checksum != iso+".sha256" || artifact.SHA256 != hex.EncodeToString(digest[:]) || artifact.SizeBytes != int64(len(contents)) {
		t.Fatalf("artifact = %#v", artifact)
	}
	if err := writeInstallerISOArtifact(&stdout, artifact); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Installer ISO ready.", "ISO: " + iso, "Metadata: " + iso + ".json", "Checksum: " + iso + ".sha256", "SHA256: " + artifact.SHA256, "Size: 30 bytes"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, stdout.String())
		}
	}
	for _, want := range []string{"building the current checkout", "verifying the completed installer ISO"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("progress missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestBuildInstallerISOStopsAfterBuildFailure(t *testing.T) {
	wantErr := errors.New("builder failed")
	calls := 0
	runner := func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
		calls++
		return wantErr
	}
	_, err := buildInstallerISO(context.Background(), t.TempDir(), io.Discard, runner)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "build installer ISO") {
		t.Fatalf("buildInstallerISO() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("runner calls = %d, want 1", calls)
	}
}

func TestBuildKatlOSUpgradeComposesAndVerifiesSupportedPipeline(t *testing.T) {
	repo := t.TempDir()
	version := "2026.7.0-dev.12"
	architecture, err := developmentArtifactArchitecture(runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	image := filepath.Join(repo, "_build", "mkosi", "katlos-upgrade-"+version+"-"+architecture+".squashfs")
	contents := []byte("current checkout KatlOS upgrade image")
	digest := sha256.Sum256(contents)
	metadata := katlosimage.ArtifactMetadata{
		APIVersion:        katlosimage.APIVersion,
		Kind:              katlosimage.ArtifactMetadataKind,
		ImageRole:         katlosimage.RoleUpgrade,
		Format:            katlosimage.FormatSquashFS,
		Version:           version,
		BuildID:           version,
		Architecture:      architecture,
		RuntimeInterface:  "katl-runtime-1",
		Path:              filepath.Base(image),
		SizeBytes:         int64(len(contents)),
		SHA256:            hex.EncodeToString(digest[:]),
		ChecksumPath:      filepath.Base(image) + ".sha256",
		EmbeddedIndexPath: "katlos/image.json",
		CreatedAt:         "2026-07-23T12:00:00Z",
	}
	type call struct {
		dir  string
		name string
		args []string
		env  []string
	}
	var calls []call
	runner := func(_ context.Context, dir, name string, args, environment []string, _, _ io.Writer) error {
		calls = append(calls, call{dir: dir, name: name, args: append([]string(nil), args...), env: append([]string(nil), environment...)})
		if err := os.MkdirAll(filepath.Dir(image), 0o755); err != nil {
			return err
		}
		metadataJSON, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		for path, data := range map[string][]byte{
			image:             contents,
			image + ".json":   append(metadataJSON, '\n'),
			image + ".sha256": []byte(metadata.SHA256 + "  " + filepath.Base(image) + "\n"),
		} {
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	artifact, err := buildKatlOSUpgrade(context.Background(), repo, version, &stderr, runner)
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []call{{
		dir:  repo,
		name: filepath.Join(repo, "scripts", "mkosi"),
		args: []string{"build-katlos-upgrade-image"},
		env:  []string{"KATL_VERSION=" + version, "KATL_UPGRADE_VERSION=" + version, "KATL_ARCHITECTURE=" + architecture, "KATL_BUILD_COMMIT=" + version},
	}}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("build calls = %#v, want %#v", calls, wantCalls)
	}
	if artifact.Path != image || artifact.Version != version || artifact.Architecture != architecture || artifact.SHA256 != metadata.SHA256 || artifact.SizeBytes != int64(len(contents)) {
		t.Fatalf("artifact = %#v", artifact)
	}
	if err := writeKatlOSUpgradeArtifact(&stdout, artifact); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"KatlOS upgrade image ready.", "Image: " + image, "katlctl node upgrade NODE --config cluster.yaml --artifact " + image} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, stdout.String())
		}
	}
	for _, internal := range []string{"SHA256:", "Checksum:", "Metadata:"} {
		if strings.Contains(stdout.String(), internal) {
			t.Fatalf("output exposed internal integrity field %q:\n%s", internal, stdout.String())
		}
	}
	for _, want := range []string{"building KatlOS " + version, "verifying the completed KatlOS upgrade image"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("progress missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestBuildKatlOSUpgradeRequiresVersion(t *testing.T) {
	_, err := buildKatlOSUpgrade(context.Background(), t.TempDir(), "", io.Discard, func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "--version is required") {
		t.Fatalf("buildKatlOSUpgrade() error = %v", err)
	}
}

func TestBuildKubernetesUpgradeComposesAndVerifiesSupportedPipeline(t *testing.T) {
	repo := t.TempDir()
	version := "v1.36.2"
	architecture, err := developmentArtifactArchitecture(runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	image := filepath.Join(repo, "_build", "mkosi", "katl-kubernetes-"+version+".raw")
	contents := []byte("current checkout Kubernetes upgrade image")
	digest := sha256.Sum256(contents)
	meta := artifact.LocalMeta{
		Name: "kubernetes", Kind: artifact.ArtifactSysext, Format: "sysext",
		Path: filepath.Base(image), SizeBytes: int64(len(contents)), SHA256: hex.EncodeToString(digest[:]),
		Generation: "checkout-build", Version: "checkout-build", PayloadVersion: version, Architecture: architecture,
		SourceRepo: &artifact.SourceRepo{ID: "kubernetes", BaseURL: "https://pkgs.k8s.io/core:/stable:/v1.36/rpm/", Minor: "v1.36"},
		PackageVersions: map[string]string{
			"kubeadm": "0:1.36.2-150500.2.1", "kubelet": "0:1.36.2-150500.2.1",
			"kubectl": "0:1.36.2-150500.2.1", "cri-tools": "0:1.36.0-150500.1.1",
		},
		RuntimeInterface:  "katl-runtime-1",
		CompatibleRuntime: &artifact.Compat{Interface: "katl-runtime-1"},
		Created:           "2026-07-24T00:00:00Z",
	}
	type call struct {
		name string
		args []string
		env  []string
	}
	var calls []call
	runner := func(_ context.Context, _ string, name string, args, environment []string, _, _ io.Writer) error {
		calls = append(calls, call{name: name, args: append([]string(nil), args...), env: append([]string(nil), environment...)})
		if filepath.Base(name) == "build-kubernetes-sysext" {
			if err := os.MkdirAll(filepath.Dir(image), 0o755); err != nil {
				return err
			}
			data, err := json.Marshal(meta)
			if err != nil {
				return err
			}
			if err := os.WriteFile(image, contents, 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(image+".json", append(data, '\n'), 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	built, err := buildKubernetesUpgrade(context.Background(), repo, "1.36.2", &stderr, runner)
	if err != nil {
		t.Fatal(err)
	}
	runtimeEnvironment := []string{
		"KATL_ARCHITECTURE=" + architecture,
		"KATL_BUILD_COMMIT=v1.36.2-katl.8",
	}
	environment := append(append([]string(nil), runtimeEnvironment...),
		"KATL_KUBERNETES_MINOR=v1.36",
		"KATL_KUBERNETES_PAYLOAD_VERSION=v1.36.2",
		"KATL_KUBERNETES_ARTIFACT_REVISION=8",
		"KATL_KUBERNETES_KUBEADM_VERSION=0:1.36.2-150500.2.1",
		"KATL_KUBERNETES_KUBELET_VERSION=0:1.36.2-150500.2.1",
		"KATL_KUBERNETES_KUBECTL_VERSION=0:1.36.2-150500.2.1",
		"KATL_KUBERNETES_CRITOOLS_VERSION=0:1.36.0-150500.1.1",
	)
	want := []call{
		{name: filepath.Join(repo, "scripts", "mkosi"), args: []string{"build-runtime"}, env: runtimeEnvironment},
		{name: filepath.Join(repo, "scripts", "build-kubernetes-sysext"), args: []string{"--output", "katl-kubernetes-v1.36.2"}, env: environment},
		{name: filepath.Join(repo, "scripts", "check-kubernetes-sysext"), args: []string{image}, env: environment},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("build calls = %#v, want %#v", calls, want)
	}
	if built.Path != image || built.PayloadVersion != version || built.Architecture != architecture || built.SHA256 != meta.SHA256 || built.SizeBytes != meta.SizeBytes {
		t.Fatalf("artifact = %#v", built)
	}
	if err := writeKubernetesBuildArtifact(&stdout, built); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Kubernetes upgrade image ready.", "Image: " + image, "katlctl kubernetes upgrade v1.36.2 --config cluster.yaml --artifact " + image} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, stdout.String())
		}
	}
	for _, internal := range []string{"SHA256:", "Checksum:", "Metadata:"} {
		if strings.Contains(stdout.String(), internal) {
			t.Fatalf("output exposed internal integrity field %q:\n%s", internal, stdout.String())
		}
	}
}

func TestBuildKubernetesUpgradeRequiresSupportedVersion(t *testing.T) {
	for _, version := range []string{"", "v1.99.0"} {
		_, err := buildKubernetesUpgrade(context.Background(), t.TempDir(), version, io.Discard, func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
			return nil
		})
		if err == nil {
			t.Fatalf("buildKubernetesUpgrade(%q) succeeded", version)
		}
	}
}

func TestInstallerIdentityIsStableAndCheckoutScoped(t *testing.T) {
	runA, macA := installerIdentity("/work/katl-a")
	runA2, macA2 := installerIdentity("/work/katl-a")
	runB, macB := installerIdentity("/work/katl-b")
	if runA != runA2 || macA != macA2 {
		t.Fatalf("identity is not stable: %q %q, %q %q", runA, macA, runA2, macA2)
	}
	if runA == runB || macA == macB {
		t.Fatalf("checkout identities collide: %q %q", runA, macA)
	}
	if !strings.HasPrefix(runA, "dev-installer-") || !strings.HasPrefix(macA, "52:54:00:") {
		t.Fatalf("identity = %q %q", runA, macA)
	}
}

func TestInstallerStateRoundTripAndReadyGuidance(t *testing.T) {
	repo := t.TempDir()
	var stdout bytes.Buffer
	manager := installerManager{repoRoot: repo, stdout: &stdout, stderr: &bytes.Buffer{}}
	state := installerState{
		APIVersion: installerStateAPIVersion,
		Kind:       installerStateKind,
		RepoRoot:   repo,
		DomainName: "katl-dev-installer-test",
		Endpoint:   "http://192.0.2.42:8080",
	}
	if err := manager.writeState(state); err != nil {
		t.Fatal(err)
	}
	loaded, err := manager.readState()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DomainName != state.DomainName || loaded.Endpoint != state.Endpoint {
		t.Fatalf("loaded state = %#v", loaded)
	}
	if err := manager.printReady(loaded); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(repo, "_build", "katldev", "cluster.yaml")
	for _, want := range []string{
		"KatlOS installer VM is ready.",
		"katlctl config init " + configPath + " --installer http://192.0.2.42:8080",
		"katlctl install apply --config " + configPath + " --endpoint http://192.0.2.42:8080",
		"katldev installer reset",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("ready output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestInstallerOptionsRejectUnsafeOrUnusableVMs(t *testing.T) {
	for _, test := range []struct {
		name string
		opts installerOptions
		want string
	}{
		{name: "memory", opts: installerOptions{MemoryMiB: 512, CPUs: 2, DiskSize: "32G", Timeout: time.Second}, want: "--memory"},
		{name: "CPUs", opts: installerOptions{MemoryMiB: 4096, DiskSize: "32G", Timeout: time.Second}, want: "--cpus"},
		{name: "disk", opts: installerOptions{MemoryMiB: 4096, CPUs: 2, Timeout: time.Second}, want: "--disk-size"},
		{name: "timeout", opts: installerOptions{MemoryMiB: 4096, CPUs: 2, DiskSize: "32G"}, want: "--timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateInstallerOptions(test.opts); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateInstallerOptions() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestInstallerAcceptingConfigStates(t *testing.T) {
	for _, state := range []string{"waiting", "waiting-for-config"} {
		if !installerAcceptingConfig(state) {
			t.Fatalf("installerAcceptingConfig(%q) = false", state)
		}
	}
	for _, state := range []string{"", "install-starting", "reboot-requested"} {
		if installerAcceptingConfig(state) {
			t.Fatalf("installerAcceptingConfig(%q) = true", state)
		}
	}
}

func TestDomainOwner(t *testing.T) {
	owner, err := domainOwner([]byte(`<vmtest xmlns="https://katlos.io/xmlns/vmtest/1">katl/katldev-installer</vmtest>`))
	if err != nil || owner != installerDomainMetadata {
		t.Fatalf("domainOwner() = %q, %v", owner, err)
	}
	if _, err := domainOwner([]byte(`<vmtest>`)); err == nil {
		t.Fatal("domainOwner() accepted malformed metadata")
	}
}
