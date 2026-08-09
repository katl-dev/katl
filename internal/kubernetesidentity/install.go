package kubernetesidentity

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var nodeSpecificPKIPaths = []string{
	"apiserver.crt",
	"apiserver.key",
	"apiserver-etcd-client.crt",
	"apiserver-etcd-client.key",
	"apiserver-kubelet-client.crt",
	"apiserver-kubelet-client.key",
	"front-proxy-client.crt",
	"front-proxy-client.key",
	"etcd/healthcheck-client.crt",
	"etcd/healthcheck-client.key",
	"etcd/peer.crt",
	"etcd/peer.key",
	"etcd/server.crt",
	"etcd/server.key",
}

// Stage stores the operator secret beside an accepted operation without
// putting private material in the operation record or journal.
func Stage(path string, data []byte) error {
	if _, err := Parse(data); err != nil {
		return err
	}
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return fmt.Errorf("Kubernetes identity staging path is required")
	}
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, data) {
			if err := os.Chmod(path, 0o600); err != nil {
				return fmt.Errorf("secure staged Kubernetes identity: %w", err)
			}
			return nil
		}
		return fmt.Errorf("staged Kubernetes identity already exists with different content")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect staged Kubernetes identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Kubernetes identity staging directory: %w", err)
	}
	return writeAtomic(path, data, 0o600)
}

// Install writes only the shared kubeadm trust and signing material. It never
// copies node-specific leaf certificates.
func Install(root string, bundle Bundle) error {
	files, err := Files(bundle)
	if err != nil {
		return err
	}
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		root = "/"
	}
	pkiDir := filepath.Join(root, "etc/kubernetes/pki")
	missing := false
	for _, file := range identityFiles {
		path := filepath.Join(pkiDir, filepath.FromSlash(file.path))
		existing, err := os.ReadFile(path)
		switch {
		case err == nil:
			if !bytes.Equal(existing, files[file.path]) {
				return fmt.Errorf("existing kubeadm PKI file /etc/kubernetes/pki/%s belongs to a different Kubernetes identity; wipe Kubernetes state or use the matching identity", file.path)
			}
		case errors.Is(err, os.ErrNotExist):
			missing = true
		default:
			return fmt.Errorf("inspect existing kubeadm PKI file /etc/kubernetes/pki/%s: %w", file.path, err)
		}
	}
	if missing {
		for _, relative := range nodeSpecificPKIPaths {
			if _, err := os.Lstat(filepath.Join(pkiDir, filepath.FromSlash(relative))); err == nil {
				return fmt.Errorf("node-specific kubeadm PKI already exists while shared identity files are incomplete; repair or wipe Kubernetes state before retrying")
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect node-specific kubeadm PKI /etc/kubernetes/pki/%s: %w", relative, err)
			}
		}
	}
	for _, file := range identityFiles {
		path := filepath.Join(pkiDir, filepath.FromSlash(file.path))
		if _, err := os.Stat(path); err == nil {
			if err := os.Chmod(path, file.mode); err != nil {
				return fmt.Errorf("secure kubeadm PKI file /etc/kubernetes/pki/%s: %w", file.path, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create kubeadm PKI directory for %s: %w", file.path, err)
		}
		if err := writeAtomic(path, files[file.path], file.mode); err != nil {
			return fmt.Errorf("install kubeadm PKI file /etc/kubernetes/pki/%s: %w", file.path, err)
		}
	}
	return nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".katl-write-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
