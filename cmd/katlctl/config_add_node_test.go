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
  managementAuthentication: trusted-network
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
			want: []string{"2 selectable disks: /dev/vda (", "--disk"},
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

// By-id aliases udev derives from on-disk metadata sort before hardware ones
// such as nvme-, and installation erases them, so recording one would break a
// reinstall.
func TestConfigAddNodeIgnoresContentDerivedByID(t *testing.T) {
	const pv = "/dev/disk/by-id/lvm-pv-uuid-Xb3kTq-0000-0000-0000-0000-0000-000000"
	for _, tt := range []struct {
		name   string
		disk   handoff.HandoffDisk
		args   []string
		record string
		refuse []string
	}{
		{
			name:   "hardware alias present",
			disk:   handoff.HandoffDisk{Path: "/dev/nvme0n1", ByID: []string{pv, "/dev/disk/by-id/nvme-Samsung_SSD_970_S1"}, Selectable: true},
			record: "byID: /dev/disk/by-id/nvme-Samsung_SSD_970_S1\n",
		},
		{
			name:   "only content aliases and a WWN",
			disk:   handoff.HandoffDisk{Path: "/dev/sda", ByID: []string{pv, "/dev/disk/by-id/dm-name-vg-root"}, WWN: "0x5000c500a1b2c3d4", Selectable: true},
			record: "wwn: \"0x5000c500a1b2c3d4\"\n",
		},
		{
			name:   "only content aliases",
			disk:   handoff.HandoffDisk{Path: "/dev/vda", ByID: []string{pv}, Selectable: true},
			refuse: []string{"no hardware by-id path, WWN, or serial"},
		},
		{
			name:   "content alias named",
			disk:   handoff.HandoffDisk{Path: "/dev/nvme0n1", ByID: []string{pv, "/dev/disk/by-id/nvme-Samsung_SSD_970_S1"}, Selectable: true},
			args:   []string{"--disk", pv},
			refuse: []string{"metadata stored on it, which installation erases", "hardware /dev/disk/by-id path"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			installer := addNodeInstaller(t, handoff.HandoffWaiting, tt.disk)
			path := writeAddNodeConfig(t, addNodeIndentedConfig)

			args := append([]string{"config", "add-node", path, "--installer", installer}, tt.args...)
			err := run(context.Background(), args, io.Discard, io.Discard)

			got := readAddNodeConfig(t, path)
			if tt.refuse != nil {
				if err == nil {
					t.Fatalf("run() succeeded, want refusal; config:\n%s", got)
				}
				for _, want := range tt.refuse {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error = %v, want %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got, "lvm-pv-uuid-") || strings.Contains(got, "dm-name-") || !strings.Contains(got, tt.record) {
				t.Fatalf("config does not record only %q:\n%s", tt.record, got)
			}
		})
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

func TestConfigAddNodeWithoutHardwareIdentity(t *testing.T) {
	identityless := handoff.HandoffDisk{Path: "/dev/vda", ByPath: []string{"/dev/disk/by-path/virtio-pci-0000:00:04.0", "/dev/disk/by-path/pci-0000:00:04.0"}, GPTGUID: "6f1e3c2a-8d4b-4f5e-9a7c-1b2d3e4f5a6b", Selectable: true}

	t.Run("not chosen implicitly", func(t *testing.T) {
		installer := addNodeInstaller(t, handoff.HandoffWaiting, identityless)
		path := writeAddNodeConfig(t, addNodeIndentedConfig)

		err := run(context.Background(), []string{"config", "add-node", path, "--installer", installer}, io.Discard, io.Discard)

		if err == nil || !strings.Contains(err.Error(), "will not choose it implicitly") || !strings.Contains(err.Error(), "byPath=/dev/disk/by-path/pci-0000:00:04.0") || !strings.Contains(err.Error(), "--disk") {
			t.Fatalf("error = %v, want refusal naming the disk's identities and --disk", err)
		}
		if got := readAddNodeConfig(t, path); got != addNodeIndentedConfig {
			t.Fatalf("refusal changed config:\n%s", got)
		}
	})

	for _, tt := range []struct {
		name     string
		disks    []handoff.HandoffDisk
		disk     string
		selector string
		warning  string
	}{
		{
			name:     "attachment path",
			disks:    []handoff.HandoffDisk{identityless},
			disk:     "/dev/disk/by-path/pci-0000:00:04.0",
			selector: "byPath: /dev/disk/by-path/pci-0000:00:04.0\n",
			warning:  "attachment slot",
		},
		{
			name: "kernel name among several disks",
			disks: []handoff.HandoffDisk{
				{Path: "/dev/sda", Selectable: true},
				{Path: "/dev/sdb", ByID: []string{"/dev/disk/by-id/ata-data"}, Selectable: true},
			},
			disk:     "/dev/sda",
			selector: "name: /dev/sda\n",
			warning:  "enumeration order",
		},
		{
			name: "kernel name of a disk with a hardware identity",
			disks: []handoff.HandoffDisk{
				{Path: "/dev/sda", Selectable: true},
				{Path: "/dev/sdb", ByID: []string{"/dev/disk/by-id/ata-data"}, Selectable: true},
			},
			disk:     "/dev/sdb",
			selector: "byID: /dev/disk/by-id/ata-data\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			installer := addNodeInstaller(t, handoff.HandoffWaiting, tt.disks...)
			path := writeAddNodeConfig(t, addNodeIndentedConfig)
			var stderr bytes.Buffer

			args := []string{"config", "add-node", path, "--installer", installer, "--disk", tt.disk}
			if err := run(context.Background(), args, io.Discard, &stderr); err != nil {
				t.Fatal(err)
			}

			if got := readAddNodeConfig(t, path); !strings.Contains(got, "systemDisk:\n                "+tt.selector) {
				t.Errorf("config does not select %q:\n%s", tt.selector, got)
			}
			warned := strings.Contains(stderr.String(), "warning: node worker-1")
			if tt.warning == "" && warned {
				t.Errorf("stderr = %q, want no weak-selector warning", stderr.String())
			}
			if tt.warning != "" && (!warned || !strings.Contains(stderr.String(), tt.warning)) {
				t.Errorf("stderr = %q, want a weak-selector warning mentioning %q", stderr.String(), tt.warning)
			}
		})
	}
}
