package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newManagementIdentityCommand(stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:     "identity",
		Short:   "Inspect and restore Katl management access",
		Long:    "Management identity is created automatically during normal install preparation. These commands are only for backup inspection and workstation recovery.",
		Example: "katlctl management identity path homelab",
	}
	command.AddCommand(newManagementIdentityPathCommand(stdout))
	command.AddCommand(newManagementIdentityInspectCommand(stdout))
	command.AddCommand(newManagementIdentityImportCommand(stdout, stderr))
	command.AddCommand(newManagementIdentityCreateCommand(stdout))
	command.AddCommand(newManagementIdentityExportCommand(stdout))
	return command
}

func newManagementIdentityPathCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:     "path CLUSTER",
		Short:   "Print the backup path for one cluster's management identity",
		Example: "katlctl management identity path homelab",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path, err := managementIdentityPath(args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, path)
			return err
		},
	}
}

func newManagementIdentityInspectCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:     "inspect IDENTITY",
		Short:   "Validate a management identity backup and print public metadata",
		Example: "katlctl management identity inspect homelab.katlkey",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			bundle, err := readManagementIdentity(args[0])
			if err != nil {
				return err
			}
			info, err := managementidentity.Validate(bundle, time.Now().UTC())
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "cluster: %s\ncreated: %s\nexpires: %s\nfingerprint: %s\nnodes: %d\n", info.ClusterName, info.CreatedAt.Format(time.RFC3339), info.ExpiresAt.Format(time.RFC3339), info.Fingerprint, len(bundle.Nodes))
			return nil
		},
	}
}

func newManagementIdentityImportCommand(stdout, _ io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:     "import IDENTITY",
		Short:   "Restore a management identity backup to this workstation",
		Example: "katlctl management identity import homelab.katlkey",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			bundle, err := readManagementIdentity(args[0])
			if err != nil {
				return err
			}
			path, err := managementIdentityPath(bundle.ClusterName)
			if err != nil {
				return err
			}
			if existing, readErr := readManagementIdentity(path); readErr == nil {
				existingInfo, validateErr := managementidentity.Validate(existing, time.Now().UTC())
				if validateErr != nil {
					return validateErr
				}
				incomingInfo, validateErr := managementidentity.Validate(bundle, time.Now().UTC())
				if validateErr != nil {
					return validateErr
				}
				if existingInfo.Fingerprint != incomingInfo.Fingerprint {
					return fmt.Errorf("management access for cluster %q already uses a different identity; refusing to replace it", bundle.ClusterName)
				}
				fmt.Fprintf(stdout, "Management access already available cluster=%s path=%s\n", bundle.ClusterName, path)
				return nil
			} else if !errors.Is(readErr, os.ErrNotExist) {
				return readErr
			}
			if err := managementidentity.Write(path, bundle); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Management access restored cluster=%s path=%s\n", bundle.ClusterName, path)
			return nil
		},
	}
}

func managementIdentityPath(clusterName string) (string, error) {
	clusterName = strings.TrimSpace(clusterName)
	if clusterName == "" || strings.ContainsAny(clusterName, `/\\`) || clusterName == "." || clusterName == ".." {
		return "", fmt.Errorf("cluster name is required")
	}
	configPath, err := workstation.ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(configPath), "management", clusterName+".katlkey"), nil
}

func managementPlanningForSource(sourcePath string, stderr io.Writer) (map[string]manifest.ManagementIdentity, error) {
	data, err := os.ReadFile(strings.TrimSpace(sourcePath))
	if err != nil {
		return nil, fmt.Errorf("read ClusterConfig %s: %w", sourcePath, err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if source.ManagementAuthentication() == managementidentity.TrustedNetwork {
		identities := make(map[string]manifest.ManagementIdentity, len(source.Spec.Nodes))
		for _, node := range source.Spec.Nodes {
			identities[node.Name] = manifest.ManagementIdentity{Authentication: managementidentity.TrustedNetwork}
		}
		return identities, nil
	}
	bundle, err := managementIdentityForSource(sourcePath, source)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	identities := make(map[string]manifest.ManagementIdentity, len(source.Spec.Nodes))
	for _, node := range source.Spec.Nodes {
		credentials, _, err := managementidentity.EnsureNode(&bundle, node.Name, now, nil)
		if err != nil {
			return nil, err
		}
		identities[node.Name] = manifest.ManagementIdentity{
			CACertificate: credentials.CACertificate, ServerCertificate: credentials.ServerCertificate, ServerPrivateKey: credentials.ServerPrivateKey,
		}
	}
	return identities, nil
}

func managementPlanningIfSource(path string, stderr io.Writer) (map[string]manifest.ManagementIdentity, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return nil, fmt.Errorf("read --config %s: %w", path, err)
	}
	if _, err := configbundle.DecodeSource(bytes.NewReader(data)); err != nil {
		return nil, nil
	}
	return managementPlanningForSource(path, stderr)
}

func managementClientForCluster(clusterName string) (managementidentity.ClientCredentials, error) {
	path, err := managementIdentityPath(clusterName)
	if err != nil {
		return managementidentity.ClientCredentials{}, err
	}
	bundle, err := readManagementIdentity(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return managementidentity.ClientCredentials{}, missingManagementIdentity(clusterName, path)
		}
		return managementidentity.ClientCredentials{}, err
	}
	if bundle.ClusterName != strings.TrimSpace(clusterName) {
		return managementidentity.ClientCredentials{}, fmt.Errorf("management identity %s belongs to cluster %q, not %q", path, bundle.ClusterName, clusterName)
	}
	return managementidentity.Client(bundle, time.Now().UTC())
}

func managementDialForEndpoint(endpoint string) (managementDialIdentity, error) {
	configPath, err := workstation.ConfigPath()
	if err != nil {
		return managementDialIdentity{}, err
	}
	config, err := workstation.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return managementDialIdentity{}, fmt.Errorf("no saved management access for endpoint %s; restore the cluster identity with 'katlctl management identity import IDENTITY', then run 'katlctl context save --config CLUSTER.yaml'", endpoint)
		}
		return managementDialIdentity{}, fmt.Errorf("load saved Katl context: %w", err)
	}
	endpoint = strings.TrimSpace(endpoint)
	var match managementDialIdentity
	for _, cluster := range config.Clusters {
		if cluster.Management == nil {
			continue
		}
		for _, node := range cluster.Nodes {
			if strings.TrimSpace(node.ManagementEndpoint) != endpoint {
				continue
			}
			if match.credentials != nil {
				return managementDialIdentity{}, fmt.Errorf("management endpoint %s belongs to more than one saved node; select a context explicitly", endpoint)
			}
			credentials := *cluster.Management
			match = managementDialIdentity{nodeName: node.Name, credentials: &credentials}
		}
	}
	if match.credentials == nil {
		return managementDialIdentity{}, fmt.Errorf("no saved management access for endpoint %s; run 'katlctl context save --config CLUSTER.yaml' or restore the cluster identity with 'katlctl management identity import IDENTITY'", endpoint)
	}
	return match, nil
}

func readManagementIdentity(path string) (managementidentity.Bundle, error) {
	info, err := os.Stat(strings.TrimSpace(path))
	if err != nil {
		return managementidentity.Bundle{}, fmt.Errorf("inspect management identity %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return managementidentity.Bundle{}, fmt.Errorf("management identity %s must be a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return managementidentity.Bundle{}, err
	}
	var document struct {
		SOPS *yaml.Node `yaml:"sops"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return managementidentity.Bundle{}, fmt.Errorf("read management secrets %s: invalid YAML", path)
	}
	if document.SOPS != nil {
		// Decrypt into memory; the durable file stays encrypted and is never
		// rewritten as a side effect of compilation or enrollment.
		data, err = exec.Command("sops", "--decrypt", "--input-type", "yaml", "--output-type", "yaml", path).Output()
		if err != nil {
			return managementidentity.Bundle{}, fmt.Errorf("decrypt management secrets %s: ensure sops is installed and its decryption key is available: %w", path, err)
		}
	} else if info.Mode().Perm()&0o077 != 0 {
		return managementidentity.Bundle{}, fmt.Errorf("management identity %s is readable by group or others (mode %04o); run 'chmod 600 %s'", path, info.Mode().Perm(), path)
	}
	bundle, err := managementidentity.Parse(data)
	return bundle, err
}
