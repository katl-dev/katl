package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/kubernetesidentity"
	"github.com/spf13/cobra"
)

type kubernetesIdentityCreateOptions struct {
	clusterName    string
	output         string
	fromKubeadmPKI string
}

func newKubernetesIdentityCommand(stdout, stderr io.Writer) *cobra.Command {
	command := &cobra.Command{
		Use:   "identity",
		Short: "Create, import, and inspect reusable Kubernetes identity",
		Long:  "Manage the operator-held CA and signing keys that let a newly provisioned cluster retain its Kubernetes trust identity.",
	}
	command.AddCommand(newKubernetesIdentityCreateCommand(stdout, stderr))
	command.AddCommand(newKubernetesIdentityInspectCommand(stdout))
	return command
}

func newKubernetesIdentityCreateCommand(stdout, stderr io.Writer) *cobra.Command {
	options := kubernetesIdentityCreateOptions{}
	command := &cobra.Command{
		Use:   "create",
		Short: "Create or import an operator-held Kubernetes identity",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runKubernetesIdentityCreate(options, stdout, stderr)
		},
	}
	command.Flags().StringVar(&options.clusterName, "cluster-name", "", "ClusterConfig metadata.name this identity belongs to")
	command.Flags().StringVar(&options.output, "output", "", "new identity file to create (stored with mode 0600)")
	command.Flags().StringVar(&options.fromKubeadmPKI, "from-kubeadm-pki", "", "import the eight shared identity files from an existing /etc/kubernetes/pki directory")
	return command
}

func runKubernetesIdentityCreate(options kubernetesIdentityCreateOptions, stdout, _ io.Writer) error {
	clusterName := strings.TrimSpace(options.clusterName)
	if clusterName == "" {
		return fmt.Errorf("--cluster-name is required and must match ClusterConfig metadata.name")
	}
	output := strings.TrimSpace(options.output)
	if output == "" {
		return fmt.Errorf("--output is required; store this file independently from the cluster nodes")
	}
	now := time.Now().UTC()
	var bundle kubernetesidentity.Bundle
	var err error
	if strings.TrimSpace(options.fromKubeadmPKI) == "" {
		bundle, err = kubernetesidentity.Generate(kubernetesidentity.GenerateOptions{ClusterName: clusterName, Now: now})
	} else {
		bundle, err = kubernetesidentity.Import(kubernetesidentity.ImportOptions{ClusterName: clusterName, CreatedAt: now, PKIDir: options.fromKubeadmPKI})
	}
	if err != nil {
		return err
	}
	if err := kubernetesidentity.Write(output, bundle); err != nil {
		return err
	}
	info, err := kubernetesidentity.Validate(bundle, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Kubernetes identity created path=%s cluster=%s fingerprint=%s\n", output, info.ClusterName, info.Fingerprint)
	fmt.Fprintln(stdout, "Store this secret independently from the cluster nodes and pass it to 'katlctl cluster bootstrap --identity'.")
	return nil
}

func newKubernetesIdentityInspectCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect IDENTITY",
		Short: "Validate an identity and print only public metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			bundle, _, err := readKubernetesIdentityFile(args[0])
			if err != nil {
				return err
			}
			info, err := kubernetesidentity.Validate(bundle, time.Now().UTC())
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "cluster: %s\n", info.ClusterName)
			fmt.Fprintf(stdout, "created: %s\n", info.CreatedAt.Format(time.RFC3339))
			fmt.Fprintf(stdout, "fingerprint: %s\n", info.Fingerprint)
			fmt.Fprintf(stdout, "kubernetes CA: %s\n", info.KubernetesCAFingerprint)
			fmt.Fprintf(stdout, "front-proxy CA: %s\n", info.FrontProxyCAFingerprint)
			fmt.Fprintf(stdout, "etcd CA: %s\n", info.EtcdCAFingerprint)
			fmt.Fprintf(stdout, "service-account key: %s\n", info.ServiceAccountKeyID)
			return nil
		},
	}
}

func readKubernetesIdentityFile(path string) (kubernetesidentity.Bundle, []byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return kubernetesidentity.Bundle{}, nil, fmt.Errorf("Kubernetes identity path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return kubernetesidentity.Bundle{}, nil, fmt.Errorf("inspect Kubernetes identity %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return kubernetesidentity.Bundle{}, nil, fmt.Errorf("Kubernetes identity %s must be a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return kubernetesidentity.Bundle{}, nil, fmt.Errorf("Kubernetes identity %s is readable by group or others (mode %04o); run 'chmod 600 %s'", path, info.Mode().Perm(), path)
	}
	return kubernetesidentity.Read(path)
}
