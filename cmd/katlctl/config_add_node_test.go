package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/handoff"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

const addNodeIndentedConfig = `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
    name: katl-lab
spec:
    # Operator note above the settings.

    managementAuthentication: trusted-network
    kubernetes:
        version: v1.36.1
    defaults:
        access:
            ssh:
                authorizedKeys:
                    - ` + uxTestSSHKey + `
    nodes:
        - name: cp-1
          controlPlane: true
          install:
            systemDisk:
                byID: /dev/disk/by-id/virtio-cp-1
          management:
            address: 10.0.0.11
          # Trailing note about cp-1.

# Footer note.
`

const addNodeCompactConfig = `apiVersion: config.katl.dev/v1alpha1
kind: ClusterConfig
metadata:
  name: katl-lab
spec:
  nodes:
  - name: cp-1  # first control plane
    controlPlane: true
    install:
      systemDisk:
        byID: /dev/disk/by-id/virtio-cp-1
    management:
      address: 10.0.0.11
  - name: worker-1
    install:
      systemDisk:
        byID: /dev/disk/by-id/virtio-worker-1
    management:
      address: 10.0.0.21
  # Settings below apply to every node.
  defaults:
    access:
      ssh:
        authorizedKeys:
        - ` + uxTestSSHKey + `
  kubernetes:
    version: v1.36.1
`

func TestConfigAddNode(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config string
		args   []string
		want   string
	}{
		{
			name:   "indented",
			config: addNodeIndentedConfig,
			want: strings.Replace(addNodeIndentedConfig, "          # Trailing note about cp-1.\n", `          # Trailing note about cp-1.
        - name: worker-1
          install:
            systemDisk:
                byID: /dev/disk/by-id/virtio-new
          management:
            address: 127.0.0.1
`, 1),
		},
		{
			name:   "compact",
			config: addNodeCompactConfig,
			args:   []string{"--name", "storage-1"},
			want: strings.Replace(addNodeCompactConfig, "      address: 10.0.0.21\n", `      address: 10.0.0.21
  - name: storage-1
    install:
      systemDisk:
        byID: /dev/disk/by-id/virtio-new
    management:
      address: 127.0.0.1
`, 1),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			installer := addNodeInstaller(t, handoff.HandoffWaiting)
			path := writeAddNodeConfig(t, tt.config)

			var stdout bytes.Buffer
			args := append([]string{"config", "add-node", path, "--installer", installer}, tt.args...)
			if err := run(context.Background(), args, &stdout, io.Discard); err != nil {
				t.Fatalf("run() error = %v", err)
			}

			if got := readAddNodeConfig(t, path); got != tt.want {
				t.Fatalf("config =\n%s\nwant\n%s", got, tt.want)
			}
			name := "worker-1"
			if len(tt.args) > 0 {
				name = tt.args[1]
			}
			for _, step := range []string{
				"katlctl install apply --config " + path + " --node " + name + " --endpoint " + installer,
				"katlctl node join " + name + " --config " + path,
			} {
				if !strings.Contains(stdout.String(), step) {
					t.Fatalf("stdout = %q, want next step %q", &stdout, step)
				}
			}
		})
	}
}

func TestConfigAddNodeRepeatIsUnchanged(t *testing.T) {
	installer := addNodeInstaller(t, handoff.HandoffWaiting)
	path := writeAddNodeConfig(t, addNodeIndentedConfig)
	args := []string{"config", "add-node", path, "--installer", installer}
	if err := run(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	added := readAddNodeConfig(t, path)

	// After install apply nothing answers as an installer, so the repeat
	// targets the bare address, where no installer listens.
	var stdout bytes.Buffer
	args = []string{"config", "add-node", path, "--installer", "127.0.0.1", "--name", "worker-1", "--role", "worker"}
	if err := run(context.Background(), args, &stdout, io.Discard); err != nil {
		t.Fatalf("repeat error = %v", err)
	}

	if got := readAddNodeConfig(t, path); got != added {
		t.Fatalf("repeat changed config:\n%s", got)
	}
	if !strings.Contains(stdout.String(), "unchanged") || !strings.Contains(stdout.String(), "katlctl node join worker-1") {
		t.Fatalf("stdout = %q, want unchanged and the next steps", &stdout)
	}
}

func TestConfigAddNodeRefusals(t *testing.T) {
	const twoNodes = addNodeIndentedConfig + `        - name: worker-1
          install:
            systemDisk:
                byID: /dev/disk/by-id/virtio-worker-1
          management:
            address: 127.0.0.1
`
	for _, tt := range []struct {
		name   string
		config string
		state  handoff.HandoffState
		disks  []handoff.HandoffDisk
		args   []string
		want   []string
	}{
		{
			name: "name taken", config: addNodeIndentedConfig,
			args: []string{"--name", "cp-1"},
			want: []string{"already has a node named cp-1 at 10.0.0.11", "choose another --name"},
		},
		{
			name: "address listed under another name", config: twoNodes,
			args: []string{"--name", "worker-9"},
			want: []string{"already lists node worker-1 at 127.0.0.1", "omit --name"},
		},
		{
			name: "address listed with another role", config: twoNodes,
			args: []string{"--role", "control-plane"},
			want: []string{"already lists worker-1 at 127.0.0.1 as a worker", "wiping and reinstalling"},
		},
		{
			name: "second control plane without endpoint", config: addNodeIndentedConfig,
			args: []string{"--role", "control-plane"},
			want: []string{"adding cp-2 would make", "invalid, so it is unchanged", "set spec.controlPlaneEndpoint.host to that address, such as 10.0.0.11 for cp-1"},
		},
		{
			name: "installer not waiting", config: addNodeIndentedConfig, state: handoff.HandoffAccepted,
			want: []string{"is not waiting for config"},
		},
		{
			name: "several disks", config: addNodeIndentedConfig,
			disks: []handoff.HandoffDisk{
				{Path: "/dev/vda", ByID: []string{"/dev/disk/by-id/virtio-new"}, Selectable: true},
				{Path: "/dev/vdb", ByID: []string{"/dev/disk/by-id/virtio-data"}, Selectable: true},
			},
			want: []string{"2 selectable disks (/dev/vda, /dev/vdb)", "--disk"},
		},
		{
			name: "unknown disk", config: addNodeIndentedConfig,
			args: []string{"--disk", "/dev/disk/by-id/virtio-missing"},
			want: []string{"no selectable disk /dev/disk/by-id/virtio-missing"},
		},
		{
			name: "flow list",
			config: strings.Replace(addNodeCompactConfig, addNodeCompactConfig[strings.Index(addNodeCompactConfig, "  nodes:"):strings.Index(addNodeCompactConfig, "  # Settings")],
				"  nodes: [{name: cp-1, controlPlane: true, install: {systemDisk: {byID: /dev/disk/by-id/virtio-cp-1}}, management: {address: 10.0.0.11}}]\n", 1),
			want: []string{"spec.nodes is not a non-empty block list", "add the node entry by hand"},
		},
		{
			name: "invalid role", config: addNodeIndentedConfig,
			args: []string{"--role", "etcd"},
			want: []string{"--role must be"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := tt.state
			if state == "" {
				state = handoff.HandoffWaiting
			}
			installer := addNodeInstaller(t, state, tt.disks...)
			path := writeAddNodeConfig(t, tt.config)

			args := append([]string{"config", "add-node", path, "--installer", installer}, tt.args...)
			err := run(context.Background(), args, io.Discard, io.Discard)

			if err == nil {
				t.Fatal("run() succeeded, want refusal")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want %q", err, want)
				}
			}
			if got := readAddNodeConfig(t, path); got != tt.config {
				t.Fatalf("refusal changed config:\n%s", got)
			}
		})
	}
}

func TestConfigAddNodeSelectsDisk(t *testing.T) {
	installer := addNodeInstaller(t, handoff.HandoffWaiting,
		handoff.HandoffDisk{Path: "/dev/vda", ByID: []string{"/dev/disk/by-id/virtio-new"}, Selectable: true},
		handoff.HandoffDisk{Path: "/dev/vdb", ByID: []string{"/dev/disk/by-id/virtio-data", "/dev/disk/by-id/wwn-data"}, Selectable: true},
	)
	path := writeAddNodeConfig(t, addNodeIndentedConfig)

	args := []string{"config", "add-node", path, "--installer", installer, "--disk", "/dev/disk/by-id/wwn-data"}
	if err := run(context.Background(), args, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	if got := readAddNodeConfig(t, path); !strings.Contains(got, "byID: /dev/disk/by-id/wwn-data\n          management:\n            address: 127.0.0.1\n") {
		t.Fatalf("config =\n%s", got)
	}
}

// addNodeInstaller serves installer status and returns its host:port. With no
// disks it reports one selectable disk, /dev/disk/by-id/virtio-new.
func addNodeInstaller(t *testing.T, state handoff.HandoffState, disks ...handoff.HandoffDisk) string {
	t.Helper()
	status := installTestStatus(state, installstatus.StateWaitingForConfig, "")
	status.Disks = disks
	if len(disks) == 0 {
		status.Disks = []handoff.HandoffDisk{{Path: "/dev/vda", ByID: []string{"/dev/disk/by-id/virtio-new"}, Selectable: true}}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(status)
	}))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func writeAddNodeConfig(t *testing.T, config string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readAddNodeConfig(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
