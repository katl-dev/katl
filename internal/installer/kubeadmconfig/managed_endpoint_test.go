package kubeadmconfig

import (
	"strings"
	"testing"
)

func TestValidateManagedEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		patches []File
		wantErr string
	}{
		{
			name:   "defaults",
			config: "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer: {}\n",
		},
		{
			name:   "explicit compatible arguments",
			config: "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer:\n  extraArgs:\n    - name: bind-address\n      value: 0.0.0.0\n    - name: secure-port\n      value: \"6443\"\n",
		},
		{
			name:    "conflicting bind address",
			config:  "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer:\n  extraArgs:\n    - name: bind-address\n      value: 192.0.2.11\n",
			wantErr: "does not accept the managed VIP",
		},
		{
			name:    "conflicting secure port",
			config:  "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\napiServer:\n  extraArgs:\n    - name: secure-port\n      value: \"7443\"\n",
			wantErr: "conflicts with the managed endpoint port 6443",
		},
		{
			name:    "compatible apiserver patch",
			config:  "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n",
			patches: []File{{RenderPath: "/etc/katl/kubeadm/cp/patches/kube-apiserver0+merge.yaml", Content: []byte("spec:\n  containers:\n    - name: kube-apiserver\n      command: [kube-apiserver, --bind-address=10.40.0.10]\n")}},
		},
		{
			name:    "conflicting apiserver patch",
			config:  "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n",
			patches: []File{{RenderPath: "/etc/katl/kubeadm/cp/patches/kube-apiserver0+merge.yaml", Content: []byte("spec:\n  containers:\n    - name: kube-apiserver\n      command: [kube-apiserver, --secure-port=7443]\n")}},
			wantErr: "conflicts with the managed endpoint port 6443",
		},
		{
			name:    "other component patch ignored",
			config:  "apiVersion: kubeadm.k8s.io/v1beta4\nkind: ClusterConfiguration\n",
			patches: []File{{RenderPath: "/etc/katl/kubeadm/cp/patches/kube-controller-manager0+merge.yaml", Content: []byte("spec:\n  containers:\n    - command: [controller, --secure-port=7443]\n")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := Plan{Name: "cp", Config: File{Content: []byte(tt.config)}, Patches: tt.patches}
			err := ValidateManagedEndpoint(plan, "10.40.0.10", 6443)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("ValidateManagedEndpoint() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("ValidateManagedEndpoint() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
