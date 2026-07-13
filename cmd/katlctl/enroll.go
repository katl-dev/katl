package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/katlctl/workstation"
	"github.com/spf13/cobra"
)

type enrollOptions struct {
	sourcePath   string
	configPath   string
	contextName  string
	sshUser      string
	identityFile string
	force        bool
}

type enrollNodeReport struct {
	Name               string `json:"name"`
	ManagementEndpoint string `json:"managementEndpoint"`
	CredentialRef      string `json:"credentialRef"`
	Connected          bool   `json:"connected"`
}

type enrollReport struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Context    string             `json:"context"`
	ConfigPath string             `json:"configPath"`
	Nodes      []enrollNodeReport `json:"nodes"`
}

var runEnrollmentSSH = func(ctx context.Context, user, address, identityFile string) ([]byte, error) {
	args := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new"}
	if strings.TrimSpace(identityFile) != "" {
		args = append(args, "-i", strings.TrimSpace(identityFile))
	}
	args = append(args, strings.TrimSpace(user)+"@"+strings.TrimSpace(address), "cat", "/var/lib/katl/agent/token")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return nil, fmt.Errorf("%w: %s", err, message)
		}
		return nil, err
	}
	return data, nil
}

func newClusterEnrollCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := enrollOptions{sshUser: "root"}
	cmd := &cobra.Command{
		Use:   "enroll SOURCE",
		Short: "Set up workstation access to installed KatlOS nodes",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			opts.sourcePath = args[0]
			return runClusterEnroll(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.configPath, "config", "", "katlctl workstation config path")
	cmd.Flags().StringVar(&opts.contextName, "context", "", "context name; defaults to the cluster name")
	cmd.Flags().StringVar(&opts.sshUser, "ssh-user", opts.sshUser, "installed-node SSH user")
	cmd.Flags().StringVar(&opts.identityFile, "identity-file", "", "SSH private key file")
	cmd.Flags().BoolVar(&opts.force, "force", false, "replace a different locally stored node token")
	return cmd
}

func runClusterEnroll(ctx context.Context, opts enrollOptions, stdout, stderr io.Writer) error {
	_ = stderr
	archive, result, err := configbundle.BuildArchive(configbundle.BuildRequest{
		SourcePath: opts.sourcePath, KatlctlVersion: version, KatlctlCommit: commit, CreatedBy: "katlctl cluster enroll",
	})
	if err != nil {
		return fmt.Errorf("compile cluster config: %w", err)
	}
	bundle, err := configbundle.ReadBundle(bytes.NewReader(archive), result.Digest)
	if err != nil {
		return fmt.Errorf("read compiled cluster config: %w", err)
	}
	inv := bundle.Manifest.Cluster.BootstrapInventory
	configPath := strings.TrimSpace(opts.configPath)
	if configPath == "" {
		configPath, err = workstation.ConfigPath()
		if err != nil {
			return err
		}
	}
	contextName := strings.TrimSpace(opts.contextName)
	if contextName == "" {
		contextName = bundle.Manifest.ClusterName
	}
	clusterProfile := workstation.Cluster{Name: bundle.Manifest.ClusterName, ControlPlaneEndpoint: inv.ControlPlaneEndpoint}
	report := enrollReport{APIVersion: "katl.dev/v1alpha1", Kind: "EnrollmentReport", Context: contextName, ConfigPath: configPath}

	for _, node := range inv.Nodes {
		credentialPath, ok := strings.CutPrefix(strings.TrimSpace(node.Access.CredentialRef), "file:")
		if !ok || strings.TrimSpace(credentialPath) == "" {
			credentialPath, err = workstation.CredentialPath(configPath, bundle.Manifest.ClusterName, node.Name)
			if err != nil {
				return err
			}
		}
		tokenData, err := runEnrollmentSSH(ctx, opts.sshUser, node.Address, opts.identityFile)
		if err != nil {
			return fmt.Errorf("enroll node %s over SSH: %w", node.Name, err)
		}
		token := strings.TrimSpace(string(tokenData))
		if token == "" || strings.ContainsAny(token, "\r\n") {
			return fmt.Errorf("enroll node %s: installed agent token is empty or malformed", node.Name)
		}
		if err := writeManagedToken(credentialPath, token, opts.force); err != nil {
			return fmt.Errorf("enroll node %s: %w", node.Name, err)
		}
		endpoint := net.JoinHostPort(strings.TrimSpace(node.Address), "9443")
		conn, err := dialKatlcAgent(ctx, endpoint, token)
		if err != nil {
			return fmt.Errorf("verify node %s management endpoint: %w", node.Name, err)
		}
		status, statusErr := conn.Client.GetNodeStatus(ctx, &agentapi.GetNodeStatusRequest{})
		closeErr := conn.Close()
		if statusErr != nil {
			return fmt.Errorf("verify node %s management endpoint: %w", node.Name, statusErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close node %s management endpoint: %w", node.Name, closeErr)
		}
		if strings.TrimSpace(status.GetMachineId()) == "" {
			return fmt.Errorf("verify node %s management endpoint: agent did not report a machine identity", node.Name)
		}
		credentialRef := "file:" + credentialPath
		clusterProfile.Nodes = append(clusterProfile.Nodes, workstation.Node{
			Name: node.Name, ManagementEndpoint: endpoint, SystemRole: node.SystemRole, CredentialRef: credentialRef,
		})
		report.Nodes = append(report.Nodes, enrollNodeReport{Name: node.Name, ManagementEndpoint: endpoint, CredentialRef: credentialRef, Connected: true})
	}

	cfg := workstation.Config{}
	if existing, loadErr := workstation.Load(configPath); loadErr == nil {
		cfg = existing
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}
	cfg = cfg.UpsertCluster(contextName, clusterProfile)
	if err := workstation.Save(configPath, cfg); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode enrollment report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func writeManagedToken(path, token string, force bool) error {
	if existing, err := os.ReadFile(path); err == nil {
		if strings.TrimSpace(string(existing)) == token {
			return os.Chmod(path, 0o600)
		}
		if !force {
			return fmt.Errorf("credential file %s contains a different token; use --force to replace it", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read credential file %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
}
