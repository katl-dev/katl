package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// assertCertificateExpiryStatus checks that a freshly bootstrapped control
// plane reports kubeadm's one-year leaf certificates through the public status
// commands, and that workers report none.
func assertCertificateExpiryStatus(t *testing.T, ctx context.Context, runDir string) error {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := runKatlctlCommand(t, ctx, katlRepoRoot(t), []string{"cluster", "status", "--output", "json"}, &stdout, &stderr); err != nil {
		return fmt.Errorf("katlctl cluster status: %w\n%s", err, stderr.String())
	}
	_ = os.WriteFile(filepath.Join(runDir, "katlctl-cluster-status.json"), stdout.Bytes(), 0o644)
	var report struct {
		Nodes []struct {
			Node       string `json:"node"`
			Kubernetes *struct {
				CertificateExpiry *struct {
					Name     string `json:"name"`
					NotAfter string `json:"notAfter"`
					State    string `json:"state"`
				} `json:"certificateExpiry"`
			} `json:"kubernetes"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		return fmt.Errorf("decode cluster status: %w\n%s", err, stdout.String())
	}
	var controlPlane string
	for _, node := range report.Nodes {
		if node.Kubernetes == nil {
			return fmt.Errorf("%s reported no Kubernetes status", node.Node)
		}
		expiry := node.Kubernetes.CertificateExpiry
		if node.Node == "worker-1" {
			if expiry != nil {
				return fmt.Errorf("worker-1 reported control-plane certificate expiry %+v", *expiry)
			}
			continue
		}
		if node.Node != "cp-1" {
			continue
		}
		if expiry == nil {
			return fmt.Errorf("cp-1 reported no certificate expiry")
		}
		notAfter, err := time.Parse(time.RFC3339, expiry.NotAfter)
		if err != nil {
			return fmt.Errorf("cp-1 certificate expiry %q: %w", expiry.NotAfter, err)
		}
		// kubeadm issues leaf certificates for one year from bootstrap.
		if remaining := time.Until(notAfter); expiry.State != "ok" || remaining < 360*24*time.Hour || remaining > 366*24*time.Hour {
			return fmt.Errorf("cp-1 certificate expiry = %+v, want ok about one year ahead", *expiry)
		}
		controlPlane = expiry.Name
	}
	if controlPlane == "" {
		return fmt.Errorf("cluster status omitted cp-1:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if err := runKatlctlCommand(t, ctx, katlRepoRoot(t), []string{"node", "status", "cp-1"}, &stdout, &stderr); err != nil {
		return fmt.Errorf("katlctl node status cp-1: %w\n%s", err, stderr.String())
	}
	if want := "certificate " + controlPlane + " expires on "; !strings.Contains(stdout.String(), want) {
		return fmt.Errorf("node status missing %q:\n%s", want, stdout.String())
	}
	return nil
}
