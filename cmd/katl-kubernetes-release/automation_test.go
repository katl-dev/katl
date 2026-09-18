package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/katl-dev/katl/internal/installer/payloadbundle"
	"github.com/katl-dev/katl/internal/installer/sysextcatalog"
	"github.com/katl-dev/katl/internal/kubernetesrelease"
	digest "github.com/opencontainers/go-digest"
)

func TestCandidateIdentity(t *testing.T) {
	packages := kubernetesrelease.PackageVersions{Kubeadm: "0:1.37.0-1", Kubelet: "0:1.37.0-1", Kubectl: "0:1.37.0-1", CRITools: "0:1.37.0-1"}
	first := candidate("v1.37.0", packages, "recipe-one")
	if repeated := candidate("v1.37.0", packages, "recipe-one"); repeated != first {
		t.Fatal("same build inputs changed identity")
	}
	if changed := candidate("v1.37.0", packages, "recipe-two"); changed.ArtifactVersion == first.ArtifactVersion {
		t.Fatal("recipe change reused immutable identity")
	}
	for _, name := range []string{"kubeadm", "kubelet", "kubectl", "cri-tools"} {
		t.Run(name, func(t *testing.T) {
			changed := packages
			switch name {
			case "kubeadm":
				changed.Kubeadm = "0:1.37.0-2"
			case "kubelet":
				changed.Kubelet = "0:1.37.0-2"
			case "kubectl":
				changed.Kubectl = "0:1.37.0-2"
			case "cri-tools":
				changed.CRITools = "0:1.37.0-2"
			}
			got := candidate("v1.37.0", changed, "recipe-one")
			if got.ArtifactVersion == first.ArtifactVersion {
				t.Fatal("package revision reused immutable identity")
			}
			if got.KubeadmVersion != changed.Kubeadm || got.KubeletVersion != changed.Kubelet || got.KubectlVersion != changed.Kubectl || got.CRIToolsVersion != changed.CRITools {
				t.Fatalf("candidate lost package locks: %+v", got)
			}
		})
	}
	if first.ArtifactRevision < 1 || first.ArtifactRevision >= 1<<53 {
		t.Fatal("revision is not lossless in JSON tooling")
	}
}

func TestCandidateRejectsPrerelease(t *testing.T) {
	query := func(string, string, string, string) (string, error) {
		t.Fatal("queried packages for invalid version")
		return "", nil
	}
	var stdout, stderr bytes.Buffer
	if err := runAutomation([]string{"candidate", "--payload-version", "v1.38.0-rc.1"}, &stdout, &stderr, query); err == nil || !strings.Contains(err.Error(), "stable") {
		t.Fatalf("error = %v", err)
	}
}

func TestPublicationRecovery(t *testing.T) {
	ctx := context.Background()
	bundle := sysextcatalog.KubernetesPayloadBundle{
		APIVersion: "payload.katl.dev/v1alpha1", Kind: "KubernetesPayloadBundle", Name: "katl-kubernetes", ArtifactKind: "katl.kubernetes-payload.v1",
		PayloadVersion: "v1.37.0", ArtifactVersion: "v1.37.0-katl.1", Architecture: "x86_64", SupportedRuntimeInterfaces: []string{"katl-runtime-1"},
	}
	var blobs []payloadbundle.Blob
	for _, role := range []string{"systemd-sysext", "sysext-metadata", "package-provenance", "catalog-fragment"} {
		blob := payloadbundle.DescribeBytes([]byte(role), role, "application/octet-stream", role)
		blobs = append(blobs, blob)
		bundle.Payloads = append(bundle.Payloads, blob.Descriptor)
	}
	config, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	packed, err := payloadbundle.Pack(ctx, payloadbundle.PackRequest{ArtifactType: sysextcatalog.KubernetesBundleArtifactType, ConfigMediaType: sysextcatalog.KubernetesBundleConfigType, Config: config, Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	manifests := map[string][]byte{"v1.37.0-katl.1": packed.Manifest, packed.ManifestDigest: packed.Manifest}
	var mu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v2/" {
			w.WriteHeader(200)
			return
		}
		const prefix = "/v2/katl/kubernetes/manifests/"
		if strings.HasPrefix(r.URL.Path, prefix) {
			reference := strings.TrimPrefix(r.URL.Path, prefix)
			if r.Method == "PUT" {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				manifests[reference] = data
				w.Header().Set("Docker-Content-Digest", digest.FromBytes(data).String())
				w.Header().Set("Location", prefix+reference)
				w.WriteHeader(201)
				return
			}
			data, exists := manifests[reference]
			if !exists {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(data).String())
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			if r.Method != "HEAD" {
				w.Write(data)
			}
			return
		}
		if r.URL.Path == "/v2/katl/kubernetes/blobs/"+digest.FromBytes(config).String() {
			w.Header().Set("Content-Length", strconv.Itoa(len(config)))
			w.Write(config)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	transport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = transport })
	repository := strings.TrimPrefix(server.URL, "https://") + "/katl/kubernetes"
	call := func(command, artifact string, extra ...string) (map[string]any, error) {
		args := []string{command, "--repository", repository, "--payload-version", "v1.37.0", "--artifact-version", artifact}
		args = append(args, extra...)
		var stdout, stderr bytes.Buffer
		err := runAutomation(args, &stdout, &stderr, nil)
		var result map[string]any
		if err == nil {
			err = json.Unmarshal(stdout.Bytes(), &result)
		}
		return result, err
	}

	missing, err := call("inspect", "v1.37.0-katl.2")
	if err != nil || missing["exists"] != false {
		t.Fatalf("missing = %v, %v", missing, err)
	}
	existing, err := call("inspect", "v1.37.0-katl.1")
	if err != nil || existing["exists"] != true || existing["promoted"] != false {
		t.Fatalf("partial = %v, %v", existing, err)
	}
	for range 2 {
		if _, err := call("promote", "v1.37.0-katl.1", "--manifest-digest", packed.ManifestDigest); err != nil {
			t.Fatal(err)
		}
	}
	complete, err := call("inspect", "v1.37.0-katl.1")
	if err != nil || complete["promoted"] != true || complete["digest"] != packed.ManifestDigest {
		t.Fatalf("complete = %v, %v", complete, err)
	}
	mu.Lock()
	actual := digest.FromBytes(manifests["compatible-v1.37.0-x86_64-katl-runtime-1"]).String()
	mu.Unlock()
	if actual != packed.ManifestDigest {
		t.Fatalf("registry promotion = %s", actual)
	}
	if _, err := call("promote", "v1.37.0-katl.2", "--manifest-digest", packed.ManifestDigest); err == nil {
		t.Fatal("promoted a digest belonging to another candidate")
	}
	mu.Lock()
	defer mu.Unlock()
	if got := digest.FromBytes(manifests["compatible-v1.37.0-x86_64-katl-runtime-1"]).String(); got != packed.ManifestDigest {
		t.Fatalf("rejected promotion changed the compatibility tag to %s", got)
	}
}
