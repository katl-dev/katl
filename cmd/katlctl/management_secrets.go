package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const defaultManagementSecrets = "management-secrets.yaml"

func missingManagementIdentity(clusterName, path string) error {
	return fmt.Errorf("management credentials for cluster %q are missing; restore the original secrets file at %s. A newly generated key cannot access installed nodes. If no backup remains, run `katlctl management identity recover --config <cluster.yaml>` with root SSH access to every node; deleting workstation context does not reset trust", clusterName, path)
}

// trustedNetworkNodes refuses to give a trusted-network cluster a new mTLS
// authority in its configuration alone. Its installed nodes keep serving
// plaintext management, so every later call with the new authority would fail.
func trustedNetworkNodes(configPath string) error {
	return fmt.Errorf("%s says managementAuthentication: trusted-network, so its installed nodes serve plaintext management and would refuse a new mTLS authority; run `katlctl management enable-mtls --config %s` to move them to mTLS. For a cluster whose nodes were never installed, set `managementAuthentication: mtls` first", configPath, configPath)
}

func contextSaveInvocation(path string) string {
	return "katlctl context save --config " + shellQuote(path)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type observedManagementKey struct{}

func loadManagementContext(ctx context.Context, path string) (workstation.Config, error) {
	if observed, ok := ctx.Value(observedManagementKey{}).(workstation.Config); ok {
		return observed, nil
	}
	return workstation.Load(path)
}

func refreshConfiguredManagement(ctx context.Context, sourcePath, contextPath, contextName string, stderr io.Writer, selectedNodes ...string) (context.Context, error) {
	return refreshConfiguredManagementAt(ctx, sourcePath, contextPath, contextName, nil, stderr, selectedNodes...)
}

// refreshConfiguredManagementAt observes nodes as refreshConfiguredManagement
// does, except that each node named in endpoints is observed at that endpoint
// instead of its configured management address.
func refreshConfiguredManagementAt(ctx context.Context, sourcePath, contextPath, contextName string, endpoints map[string]inventory.HostPort, stderr io.Writer, selectedNodes ...string) (context.Context, error) {
	if strings.TrimSpace(sourcePath) == "" {
		return ctx, nil
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return ctx, fmt.Errorf("read --config %s: %w", sourcePath, err)
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		bundle, bundleErr := configbundle.ReadBundle(bytes.NewReader(data), "")
		if bundleErr != nil || bundle.Authentication != managementidentity.TrustedNetwork {
			if bundleErr == nil {
				return withSavedEndpoints(ctx, bundle.Manifest.ClusterName, endpoints)
			}
			return ctx, nil
		}
	} else {
		authentication, err := source.ManagementAuthentication()
		if err != nil {
			return ctx, err
		}
		if authentication == managementidentity.MutualTLS && source.Spec.ManagementIdentity == "" {
			return withSavedEndpoints(ctx, source.Metadata.Name, endpoints)
		}
	}
	// Instance IDs bind this operation to the observed installations. Only an
	// explicit context save persists observations or changes the current cluster.
	if ctx.Value(observedManagementKey{}) == nil {
		ctx = context.WithValue(ctx, observedManagementKey{}, workstation.Config{})
	}
	observed, _, err := prepareContext(ctx, contextSaveOptions{configInput: sourcePath, contextPath: contextPath, contextName: contextName, timeout: defaultManagementTimeout, output: "text", selectedNodes: selectedNodes, endpoints: endpoints, endpointSource: addressSupplied}, "the same command")
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, observedManagementKey{}, observed), nil
}

// withSavedEndpoints overlays endpoints on the saved context for clusterName
// for this command only; the saved file is unchanged.
func withSavedEndpoints(ctx context.Context, clusterName string, endpoints map[string]inventory.HostPort) (context.Context, error) {
	if len(endpoints) == 0 {
		return ctx, nil
	}
	path, err := workstation.ConfigPath()
	if err != nil {
		return ctx, err
	}
	saved, err := loadManagementContext(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return ctx, nil
	}
	if err != nil {
		return ctx, err
	}
	for i := range saved.Clusters {
		if strings.TrimSpace(saved.Clusters[i].Name) != strings.TrimSpace(clusterName) {
			continue
		}
		nodes := slices.Clone(saved.Clusters[i].Nodes)
		for j := range nodes {
			if endpoint, ok := endpoints[nodes[j].Name]; ok {
				nodes[j].ManagementEndpoint = endpoint.String()
			}
		}
		saved.Clusters[i].Nodes = nodes
	}
	return context.WithValue(ctx, observedManagementKey{}, saved), nil
}

func managementIdentityForSource(sourcePath string, source configbundle.SourceConfig) (managementidentity.Bundle, error) {
	path := strings.TrimSpace(source.Spec.ManagementIdentity)
	if path == "" {
		var err error
		path, err = managementIdentityPath(source.Metadata.Name)
		if err != nil {
			return managementidentity.Bundle{}, err
		}
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(sourcePath), path)
	}
	bundle, err := readManagementIdentityForCluster(path, source.Metadata.Name)
	if errors.Is(err, os.ErrNotExist) {
		return managementidentity.Bundle{}, missingManagementIdentity(source.Metadata.Name, path)
	}
	return bundle, err
}

func readManagementIdentityForCluster(path, clusterName string) (managementidentity.Bundle, error) {
	bundle, err := readManagementIdentity(path)
	if err != nil {
		return managementidentity.Bundle{}, err
	}
	if bundle.ClusterName != clusterName {
		return managementidentity.Bundle{}, fmt.Errorf("management secrets %s belong to cluster %q, not %q", path, bundle.ClusterName, clusterName)
	}
	return bundle, nil
}

func managementClientForConfig(path, clusterName string) (managementidentity.ClientCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return managementidentity.ClientCredentials{}, err
	}
	source, err := configbundle.DecodeSource(bytes.NewReader(data))
	if err != nil {
		bundle, bundleErr := configbundle.ReadBundle(bytes.NewReader(data), "")
		if bundleErr != nil {
			return managementidentity.ClientCredentials{}, bundleErr
		}
		if bundle.Authentication == managementidentity.TrustedNetwork {
			return managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}, nil
		}
		return managementClientForCluster(clusterName)
	}
	authentication, err := source.ManagementAuthentication()
	if err != nil {
		return managementidentity.ClientCredentials{}, err
	}
	if authentication == managementidentity.TrustedNetwork {
		return managementidentity.ClientCredentials{Authentication: managementidentity.TrustedNetwork}, nil
	}
	bundle, err := managementIdentityForSource(path, source)
	if err != nil {
		return managementidentity.ClientCredentials{}, err
	}
	return managementidentity.Client(bundle, time.Now().UTC())
}

func newManagementIdentityCreateCommand(stdout io.Writer) *cobra.Command {
	return managementSecretsCommand("create", "Create management secrets for a new cluster", stdout, false)
}

func newManagementIdentityExportCommand(stdout io.Writer) *cobra.Command {
	return managementSecretsCommand("export", "Move existing cluster management secrets beside its configuration", stdout, true)
}

func managementSecretsCommand(action, short string, stdout io.Writer, export bool) *cobra.Command {
	var configPath, outputPath string
	command := &cobra.Command{
		Use: action, Short: short, Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			data, err := os.ReadFile(configPath)
			if err != nil {
				return fmt.Errorf("read --config %s: %w", configPath, err)
			}
			source, err := configbundle.DecodeSource(bytes.NewReader(data))
			if err != nil {
				return err
			}
			// Both commands rewrite the config to mTLS. A new authority also needs
			// the mode stated, because the nodes of a config without one may be
			// installed with trusted-network management.
			authentication, modeErr := source.ManagementAuthentication()
			if authentication == managementidentity.TrustedNetwork {
				return trustedNetworkNodes(configPath)
			}
			if modeErr != nil && !export {
				return modeErr
			}
			if outputPath == "" {
				name := source.Spec.ManagementIdentity
				if name == "" {
					name = defaultManagementSecrets
				}
				outputPath = name
				if !filepath.IsAbs(outputPath) {
					outputPath = filepath.Join(filepath.Dir(configPath), outputPath)
				}
			}
			var identity managementidentity.Bundle
			if export {
				identity, err = managementIdentityForSource(configPath, source)
			} else {
				identity, err = managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: source.Metadata.Name})
			}
			if err != nil {
				return err
			}
			for _, node := range source.Spec.Nodes {
				if _, _, err := managementidentity.EnsureNode(&identity, node.Name, time.Now().UTC(), nil); err != nil {
					return err
				}
			}
			if err := managementidentity.Write(outputPath, identity); err != nil {
				return err
			}
			if err := referenceManagementSecrets(configPath, outputPath, data); err != nil {
				return fmt.Errorf("secrets saved at %s, but could not update the config reference: %w", outputPath, err)
			}
			fmt.Fprintf(stdout, "Saved cluster management secrets to %s and referenced them from %s\n", outputPath, configPath)
			fmt.Fprintln(stdout, "Keep this file across reinstalls; it may be SOPS encrypted before committing to version control.")
			return nil
		},
	}
	command.Flags().StringVar(&configPath, "config", "", "ClusterConfig YAML")
	command.Flags().StringVar(&outputPath, "output", "", "secrets output file; defaults beside the config")
	_ = command.MarkFlagRequired("config")
	return command
}

func referenceManagementSecrets(configPath, secretsPath string, original []byte) error {
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	secretsPath, err = filepath.Abs(secretsPath)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(filepath.Dir(configPath), secretsPath)
	if err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(original, &document); err != nil {
		return err
	}
	root := document.Content[0]
	var spec *yaml.Node
	for index := 0; index < len(root.Content); index += 2 {
		if root.Content[index].Value == "spec" {
			spec = root.Content[index+1]
			break
		}
	}
	if spec == nil {
		return fmt.Errorf("config has no spec")
	}
	var reference *yaml.Node
	for index := 0; index < len(spec.Content); index += 2 {
		if spec.Content[index].Value == "managementIdentity" {
			reference = spec.Content[index+1]
			break
		}
	}
	if reference == nil {
		reference = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str"}
		spec.Content = append(spec.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "managementIdentity"}, reference)
	}
	var authentication *yaml.Node
	for index := 0; index < len(spec.Content); index += 2 {
		if spec.Content[index].Value == "managementAuthentication" {
			authentication = spec.Content[index+1]
			break
		}
	}
	if authentication == nil {
		authentication = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str"}
		spec.Content = append(spec.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "managementAuthentication"}, authentication)
	}
	authentication.Value = string(managementidentity.MutualTLS)
	reference.Value = filepath.ToSlash(relative)
	data, err := yaml.Marshal(&document)
	if err != nil {
		return err
	}
	// Publish the complete reference atomically, never a truncated config.
	temporary, err := os.CreateTemp(filepath.Dir(configPath), ".katl-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	current, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, original) {
		return fmt.Errorf("config changed while creating secrets; set spec.managementIdentity to %q", relative)
	}
	return os.Rename(temporary.Name(), configPath)
}
