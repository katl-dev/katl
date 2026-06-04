package confext

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidateNativeEtcBundleAcceptsKnownConfigPaths(t *testing.T) {
	plans, err := ValidateNativeEtcBundle("/target/var/lib/katl/generations/2026.05.31-001/confext", []NativeEtcFile{
		{Path: "/etc/systemd/network/10-lan.network", Mode: 0o644},
		{Path: "/etc/ssh/sshd_config.d/10-katl.conf", Mode: 0o600},
		{Path: "/etc/katl/kubeadm-init.yaml", Mode: 0o640},
	})
	if err != nil {
		t.Fatalf("ValidateNativeEtcBundle() error = %v", err)
	}
	if len(plans) != 3 {
		t.Fatalf("len(plans) = %d, want 3", len(plans))
	}
	for _, plan := range plans {
		if !strings.HasPrefix(plan.ConfextPath, "/target/var/lib/katl/generations/2026.05.31-001/confext/etc/") {
			t.Fatalf("ConfextPath = %q, want path under generated confext root", plan.ConfextPath)
		}
		if plan.UID != 0 || plan.GID != 0 {
			t.Fatalf("ownership = %d:%d, want root:root", plan.UID, plan.GID)
		}
	}
}

func TestValidateNativeEtcBundleRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name string
		file NativeEtcFile
		want string
	}{
		{
			name: "relative path",
			file: NativeEtcFile{Path: "etc/hostname"},
			want: "must be absolute",
		},
		{
			name: "outside etc",
			file: NativeEtcFile{Path: "/var/lib/katl/node.yaml"},
			want: "must be under /etc",
		},
		{
			name: "path traversal",
			file: NativeEtcFile{Path: "/etc/../root/.ssh/authorized_keys"},
			want: "contains path traversal",
		},
		{
			name: "kubernetes ownership",
			file: NativeEtcFile{Path: "/etc/kubernetes/admin.conf"},
			want: "cannot own kubeadm-managed",
		},
		{
			name: "extension metadata ownership",
			file: NativeEtcFile{Path: "/etc/extension-release.d/extension-release.katl-node"},
			want: "cannot own generated confext extension-release metadata",
		},
		{
			name: "symlink",
			file: NativeEtcFile{Path: "/etc/hostname", Type: NativeEtcSymlink},
			want: "symlink entries are not allowed",
		},
		{
			name: "device node",
			file: NativeEtcFile{Path: "/etc/hostname", Type: NativeEtcCharDevice},
			want: "char-device entries are not allowed",
		},
		{
			name: "mode type symlink",
			file: NativeEtcFile{Path: "/etc/hostname", Mode: fs.ModeSymlink | 0o777},
			want: "must be a regular file",
		},
		{
			name: "world writable",
			file: NativeEtcFile{Path: "/etc/hostname", Mode: 0o666},
			want: "cannot be group- or world-writable",
		},
		{
			name: "executable",
			file: NativeEtcFile{Path: "/etc/profile.d/katl.sh", Mode: 0o755},
			want: "cannot be executable",
		},
		{
			name: "non root owner",
			file: NativeEtcFile{Path: "/etc/hostname", UID: 1000},
			want: "must be owned by root:root",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateNativeEtcBundle("/target/confext", []NativeEtcFile{tt.file})
			if err == nil {
				t.Fatalf("ValidateNativeEtcBundle() error = nil, want %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestValidateNativeEtcBundleRejectsDuplicateNormalizedPaths(t *testing.T) {
	_, err := ValidateNativeEtcBundle("/target/confext", []NativeEtcFile{
		{Path: "/etc/hostname"},
		{Path: "/etc/./hostname"},
	})
	if err == nil {
		t.Fatal("ValidateNativeEtcBundle() error = nil, want duplicate path error")
	}
	if !strings.Contains(err.Error(), "duplicate /etc file path") {
		t.Fatalf("error = %q, want duplicate path error", err)
	}
}

func TestValidateNativeEtcBundleRequiresAbsoluteConfextRoot(t *testing.T) {
	_, err := ValidateNativeEtcBundle("relative/confext", []NativeEtcFile{{Path: "/etc/hostname"}})
	if err == nil {
		t.Fatal("ValidateNativeEtcBundle() error = nil, want absolute root error")
	}
	if !strings.Contains(err.Error(), "confext root must be absolute") {
		t.Fatalf("error = %q, want absolute root error", err)
	}
}

func TestRenderGenerationTreeWritesFilesAndMetadata(t *testing.T) {
	var chowns []string
	tree, err := RenderGenerationTree(GenerationTreeRequest{
		GenerationsRoot: t.TempDir(),
		GenerationID:    "2026.05.31-001",
		Files: []NativeEtcFile{
			{Path: "/etc/ssh/sshd_config.d/10-katl.conf", Content: "PasswordAuthentication no\n", Mode: 0o600},
			{Path: "/etc/systemd/network/10-lan.network", Content: "[Match]\nName=enp1s0\n", Mode: 0o644},
			{Path: "/etc/katl/node.yaml", Content: "node: lab-node-01\n", Mode: 0o640},
		},
		Extension: ExtensionRelease{
			Name:         "katl-node",
			ID:           "katl",
			VersionID:    "0.1.0",
			ConfextLevel: 1,
		},
		Chown: func(path string, uid int, gid int) error {
			chowns = append(chowns, filepath.Base(path)+":0:0")
			if uid != 0 || gid != 0 {
				t.Fatalf("chown %s = %d:%d, want 0:0", path, uid, gid)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("RenderGenerationTree() error = %v", err)
	}

	wantFiles := []string{
		"/etc/katl/node.yaml",
		"/etc/ssh/sshd_config.d/10-katl.conf",
		"/etc/systemd/network/10-lan.network",
	}
	var gotFiles []string
	for _, file := range tree.Files {
		gotFiles = append(gotFiles, file.Path)
		if !strings.HasPrefix(file.ConfextPath, tree.ConfextDir+string(os.PathSeparator)+"etc"+string(os.PathSeparator)) {
			t.Fatalf("ConfextPath = %q, want path under %s/etc", file.ConfextPath, tree.ConfextDir)
		}
	}
	if !reflect.DeepEqual(gotFiles, wantFiles) {
		t.Fatalf("rendered files = %#v, want %#v", gotFiles, wantFiles)
	}

	assertFile(t, filepath.Join(tree.ConfextDir, "etc", "katl", "node.yaml"), "node: lab-node-01\n", 0o640)
	assertFile(t, filepath.Join(tree.ConfextDir, "etc", "ssh", "sshd_config.d", "10-katl.conf"), "PasswordAuthentication no\n", 0o600)
	assertFile(t, filepath.Join(tree.ConfextDir, "etc", "systemd", "network", "10-lan.network"), "[Match]\nName=enp1s0\n", 0o644)
	assertFile(t, tree.ExtensionReleasePath, "ID=katl\nVERSION_ID=0.1.0\nCONFEXT_LEVEL=1\n", 0o644)

	wantChowns := []string{
		"node.yaml:0:0",
		"10-katl.conf:0:0",
		"10-lan.network:0:0",
		"extension-release.katl-node:0:0",
	}
	if !reflect.DeepEqual(chowns, wantChowns) {
		t.Fatalf("chowns = %#v, want %#v", chowns, wantChowns)
	}
}

func TestRenderGenerationTreeRejectsUnsafeGenerationInput(t *testing.T) {
	tests := []struct {
		name    string
		request GenerationTreeRequest
		want    string
	}{
		{
			name:    "relative generations root",
			request: GenerationTreeRequest{GenerationsRoot: "var/lib/katl/generations", GenerationID: "good"},
			want:    "generations root must be absolute",
		},
		{
			name:    "path traversal generation",
			request: GenerationTreeRequest{GenerationsRoot: t.TempDir(), GenerationID: "../bad"},
			want:    "must be a single path segment",
		},
		{
			name: "unsafe file path",
			request: GenerationTreeRequest{
				GenerationsRoot: t.TempDir(),
				GenerationID:    "good",
				Files:           []NativeEtcFile{{Path: "/etc/kubernetes/admin.conf"}},
				Chown:           func(string, int, int) error { return nil },
			},
			want: "cannot own kubeadm-managed",
		},
		{
			name: "invalid extension name",
			request: GenerationTreeRequest{
				GenerationsRoot: t.TempDir(),
				GenerationID:    "good",
				Extension:       ExtensionRelease{Name: "../bad"},
				Chown:           func(string, int, int) error { return nil },
			},
			want: "extension-release name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := RenderGenerationTree(tt.request)
			if err == nil {
				t.Fatalf("RenderGenerationTree() error = nil, want %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want substring %q", err, tt.want)
			}
		})
	}
}

func TestRenderGenerationTreeRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	confextDir := filepath.Join(root, "good", "confext")
	if err := os.MkdirAll(confextDir, 0o755); err != nil {
		t.Fatalf("mkdir confext: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(confextDir, "etc")); err != nil {
		t.Fatalf("symlink etc: %v", err)
	}

	_, err := RenderGenerationTree(GenerationTreeRequest{
		GenerationsRoot: root,
		GenerationID:    "good",
		Files:           []NativeEtcFile{{Path: "/etc/systemd/network/10-lan.network", Content: "[Match]\nName=enp1s0\n"}},
		Chown:           func(string, int, int) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to follow symlink") {
		t.Fatalf("RenderGenerationTree() error = %v, want symlink rejection", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "systemd/network/10-lan.network")); !os.IsNotExist(err) {
		t.Fatalf("outside write err = %v, want no escaped write", err)
	}
}

func TestRenderGenerationTreeRejectsHardlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	outsideFile := filepath.Join(outside, "hostname")
	if err := os.WriteFile(outsideFile, []byte("outside\n"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	targetPath := filepath.Join(root, "good", "confext", "etc", "hostname")
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("mkdir target parent: %v", err)
	}
	if err := os.Link(outsideFile, targetPath); err != nil {
		t.Fatalf("hardlink target: %v", err)
	}

	_, err := RenderGenerationTree(GenerationTreeRequest{
		GenerationsRoot: root,
		GenerationID:    "good",
		Files:           []NativeEtcFile{{Path: "/etc/hostname", Content: "inside\n"}},
		Chown:           func(string, int, int) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite hard-linked") {
		t.Fatalf("RenderGenerationTree() error = %v, want hardlink rejection", err)
	}
	data, err := os.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("read outside: %v", err)
	}
	if string(data) != "outside\n" {
		t.Fatalf("outside file was modified: %q", data)
	}
}

func assertFile(t *testing.T, path string, wantContent string, wantMode fs.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != wantContent {
		t.Fatalf("%s content = %q, want %q", path, data, wantContent)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != wantMode {
		t.Fatalf("%s mode = %04o, want %04o", path, got, wantMode)
	}
}
