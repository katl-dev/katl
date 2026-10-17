package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

// Captured from the shipped 16ca4b1475dc decoder through
// configapply.DecodeNodeConfigurationChange, also used by agent validation.
const oldSharedKubeletBaseError = "decode node configuration change: yaml: unmarshal errors:\n  line 12: field sharedKubeletBase not found in type configapply.inlineKubeadmConfig"

func TestNodeApplyRuntimeCompatibility(t *testing.T) {
	for _, test := range []struct {
		name, diagnostic                  string
		args                              []string
		transportError, guidance, receipt bool
	}{
		{name: "decoder", diagnostic: oldSharedKubeletBaseError, transportError: true, guidance: true},
		{name: "other field", diagnostic: strings.ReplaceAll(oldSharedKubeletBaseError, "sharedKubeletBase", "otherField"), transportError: true},
		{name: "other type", diagnostic: strings.ReplaceAll(oldSharedKubeletBaseError, "inlineKubeadmConfig", "otherConfig"), transportError: true},
		{name: "rejected", diagnostic: oldSharedKubeletBaseError, guidance: true},
		{name: "plan text", diagnostic: oldSharedKubeletBaseError, args: []string{"--plan"}, guidance: true},
		{name: "plan JSON", diagnostic: oldSharedKubeletBaseError, args: []string{"--plan", "--output", "json"}, guidance: true},
		{name: "explicit rejected receipt", diagnostic: oldSharedKubeletBaseError, args: []string{"--mode", "live", "--output", "json"}, guidance: true, receipt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			contextPath := writeNodeAEnrollmentContext(t)
			path := filepath.Join(t.TempDir(), "change.yaml")
			if err := os.WriteFile(path, []byte("apiVersion: katl.dev/v1alpha1\nkind: NodeConfigurationChange\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			base := &fakeKatlcAgentClient{
				nodeStatus:     enrolledNodeAStatus(),
				validateResult: &agentapi.ConfigValidationResult{FailureReason: "configuration rejected", Diagnostics: []string{test.diagnostic}, RequestDigest: "private-digest"},
				stageAccepted:  &agentapi.OperationAccepted{OperationId: "rejected", InitialStatus: &agentapi.OperationStatus{Terminal: true, Result: "failed", FailureReason: test.diagnostic}},
			}
			client := &compatibilityValidationClient{fakeKatlcAgentClient: base}
			if test.transportError {
				client.err = errors.New(test.diagnostic)
			}
			oldDial := dialKatlcAgent
			t.Cleanup(func() { dialKatlcAgent = oldDial })
			dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}
			args := append([]string{"node", "apply", "--context-file", contextPath, "--endpoint", "node-a.example.test:9443", "--config", path}, test.args...)
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), args, &stdout, &stderr)
			message := stdout.String() + stderr.String()
			if err != nil {
				message += err.Error()
			}
			if strings.Contains(message, "upgrade the node runtime") != test.guidance || !strings.Contains(message, "sharedKubeletBase") && test.guidance {
				t.Fatalf("compatibility diagnostic = %q, error = %v", message, err)
			}
			if client.err != nil && !errors.Is(err, client.err) {
				t.Fatalf("underlying decoder error lost: %v", err)
			}
			if (base.applyRequest != nil) != test.receipt || base.stageRequest != nil || len(base.submitRequests) != 0 {
				t.Fatalf("unexpected submission: apply=%+v stage=%+v submit=%v", base.applyRequest, base.stageRequest, base.submitRequests)
			}
			if test.name == "plan JSON" {
				var result agentapi.ConfigValidationResult
				if err := protojson.Unmarshal(stdout.Bytes(), &result); err != nil || result.Accepted || result.RequestDigest != "" || !strings.Contains(result.FailureReason, "upgrade the node runtime") {
					t.Fatalf("plan result = %s, error=%v", stdout.String(), err)
				}
			}
			if test.receipt {
				var receipt agentapi.OperationStatus
				if err := protojson.Unmarshal(stdout.Bytes(), &receipt); err != nil || receipt.Result != "failed" || !receipt.Terminal {
					t.Fatalf("rejected receipt = %s, error=%v", stdout.String(), err)
				}
			}
			if base.validateResult.FailureReason != "configuration rejected" {
				t.Fatal("client modified the agent validation result")
			}
		})
	}
}

func TestClusterApplyRuntimeCompatibility(t *testing.T) {
	for _, transportError := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected result", true: "decoder error"}[transportError], func(t *testing.T) {
			path := writeClusterConfig(t)
			base := &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{MachineId: "machine-cp-1", CurrentGenerationId: "0"}, validateResult: &agentapi.ConfigValidationResult{Diagnostics: []string{oldSharedKubeletBaseError}}}
			client := &compatibilityValidationClient{fakeKatlcAgentClient: base}
			if transportError {
				client.err = errors.New(oldSharedKubeletBaseError)
			}
			oldDial := dialKatlcAgent
			t.Cleanup(func() { dialKatlcAgent = oldDial })
			dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
				return katlcAgentConnection{Client: client, Close: func() error { return nil }}, nil
			}
			_, err := activateClusterConfig(context.Background(), kubeadmControlPlaneConfigOptions{configPath: path, rolloutID: "compatibility"}, []inventory.Node{{Name: "cp-1", Address: "10.0.0.11", SystemRole: inventory.RoleControlPlane}})
			if err == nil || !strings.Contains(err.Error(), "upgrade the node runtime") || !strings.Contains(err.Error(), oldSharedKubeletBaseError) {
				t.Fatalf("cluster compatibility rejection = %v", err)
			}
			if client.err != nil && !errors.Is(err, client.err) {
				t.Fatalf("underlying decoder error lost: %v", err)
			}
			if len(base.submitRequests) != 0 || base.applyRequest != nil || base.stageRequest != nil {
				t.Fatal("cluster validation rejection submitted configuration")
			}
		})
	}
}

func TestInstallBundleCompatibilityDiagnostic(t *testing.T) {
	for _, test := range []struct {
		name, message string
		status        int
		guidance      bool
	}{
		{name: "old installer", status: 400, message: `invalid config bundle: config bundle requires unsupported installer feature "shared-kubelet-base-v1"`, guidance: true},
		{name: "other feature", status: 400, message: `invalid config bundle: config bundle requires unsupported installer feature "other-v1"`},
		{name: "other rejection", status: 400, message: "invalid config bundle: digest mismatch"},
		{name: "server error", status: 500, message: `invalid config bundle: config bundle requires unsupported installer feature "shared-kubelet-base-v1"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				posts++
				if request.Method != http.MethodPost || request.URL.Path != "/v1/config-bundle" {
					t.Errorf("unexpected request %s %s", request.Method, request.URL)
				}
				http.Error(w, test.message, test.status)
			}))
			defer server.Close()
			_, err := submitInstallBundle(context.Background(), server.Client(), server.URL, []byte("bundle"), "digest", "cp-1", nil)
			if err == nil || !strings.Contains(err.Error(), test.message) || strings.Contains(err.Error(), "boot installer media") != test.guidance || posts != 1 {
				t.Fatalf("posts=%d error=%v", posts, err)
			}
		})
	}

	t.Run("network failure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := submitInstallBundle(ctx, http.DefaultClient, "http://127.0.0.1:1", []byte("bundle"), "digest", "cp-1", nil)
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "boot installer media") {
			t.Fatalf("network failure changed: %v", err)
		}
	})
}

type compatibilityValidationClient struct {
	*fakeKatlcAgentClient
	err error
}

func (c *compatibilityValidationClient) ValidateConfig(ctx context.Context, request *agentapi.ValidateConfigRequest, opts ...grpc.CallOption) (*agentapi.ConfigValidationResult, error) {
	result, _ := c.fakeKatlcAgentClient.ValidateConfig(ctx, request, opts...)
	return result, c.err
}
