package scriptstest

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBaseContainerRuntimeContractIsEnforced(t *testing.T) {
	repo := repoRoot(t)

	runtimeProfile := string(mustReadFile(t, filepath.Join(repo, "mkosi.profiles", "runtime", "mkosi.conf")))
	runtimePackages := mkosiPackages(runtimeProfile)
	for _, pkg := range []string{"containerd", "crun", "containernetworking-plugins"} {
		if !runtimePackages[pkg] {
			t.Fatalf("runtime mkosi profile missing base container runtime package %q", pkg)
		}
	}

	kubernetesProfile := string(mustReadFile(t, filepath.Join(repo, "mkosi.profiles", "kubernetes-sysext", "mkosi.conf")))
	kubernetesPackages := mkosiPackages(kubernetesProfile)
	for _, pkg := range []string{"containerd", "crun", "containernetworking-plugins"} {
		if kubernetesPackages[pkg] {
			t.Fatalf("kubernetes sysext profile must not own base container runtime package %q", pkg)
		}
	}

	runtimeCheck := string(mustReadFile(t, filepath.Join(repo, "scripts", "check-runtime-root")))
	assertTextContains(t, runtimeCheck,
		"/usr/bin/containerd",
		"/usr/bin/crun",
		"/usr/libexec/cni/bridge",
		"/usr/libexec/cni/host-local",
		"/usr/libexec/cni/loopback",
		"/usr/lib/systemd/system/containerd.service",
		"/usr/lib/systemd/system/containerd.service.d/10-katl-runtime.conf",
		"Requires=systemd-sysext.service systemd-confext.service containerd.service kubelet.service etc-kubernetes.mount katl-state-projection-check.service katlc-agent.service",
		"RequiresMountsFor=/var/lib/containerd",
		"ReadWritePaths=/run /efi /etc/kubernetes /var/lib/containerd /var/lib/etcd /var/lib/katl /var/lib/kubelet /var/log/journal",
	)

	installerCheck := string(mustReadFile(t, filepath.Join(repo, "scripts", "check-installer-image")))
	assertTextContains(t, installerCheck,
		"/usr/bin/containerd",
		"/usr/bin/crun",
		"/usr/lib/systemd/system/containerd.service",
	)

	surfaceDoc := string(mustReadFile(t, filepath.Join(repo, "docs", "internal", "v0.1-supported-image-surface.md")))
	assertTextContains(t, surfaceDoc,
		"`containerd`, `crun` | internal Kubernetes runtime | managed by Katl/systemd, not a user container platform.",
		"CNI binaries under `/usr/libexec/cni` | internal Kubernetes prerequisites | not a supported CNI management interface.",
		"`containerd.service` | supported internal | Kubernetes/container runtime dependency.",
	)

	adr := string(mustReadFile(t, filepath.Join(repo, "docs", "internal", "adrs", "adr-005-container-runtime-extension-boundary.md")))
	assertTextContains(t, adr,
		"containerd",
		"the selected OCI runtime, such as crun or runc",
		"persistent state projection for /var/lib/containerd",
		"Latest supported",
		"must not present that as the managed cluster CNI",
	)
}

func TestReleaseProfilesPinTrustPackages(t *testing.T) {
	repo := repoRoot(t)
	wants := []string{
		"libacl-0:2.3.2-6.fc44.x86_64",
		"libattr-0:2.5.2-8.fc44.x86_64",
		"p11-kit-0:0.26.2-1.fc44.x86_64",
		"p11-kit-trust-0:0.26.2-1.fc44.x86_64",
	}
	for _, profile := range []string{"runtime", "installer-image"} {
		config := string(mustReadFile(t, filepath.Join(repo, "mkosi.profiles", profile, "mkosi.conf")))
		packages := mkosiPackages(config)
		for _, want := range wants {
			if !packages[want] {
				t.Errorf("%s mkosi profile missing pinned package %q", profile, want)
			}
		}
	}
}

func mkosiPackages(config string) map[string]bool {
	packages := map[string]bool{}
	inPackages := false
	for _, line := range strings.Split(config, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "Packages=":
			inPackages = true
			continue
		case strings.HasSuffix(trimmed, "=") && !strings.HasPrefix(trimmed, "#"):
			inPackages = false
		}
		if !inPackages || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		for _, field := range strings.Fields(trimmed) {
			packages[field] = true
		}
	}
	return packages
}

func assertTextContains(t *testing.T, text string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q", want)
		}
	}
}
