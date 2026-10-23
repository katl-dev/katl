package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestBootstrapProgressSummary(t *testing.T) {
	var out bytes.Buffer
	progress := bootstrapProgressWriter(&out, false)
	event := cluster.AgentBootstrapProgress{Node: "cp-1", Kind: "bootstrap-init", Phase: "kubeadm-init", OperationID: "internal-id"}
	progress(event)
	progress(event)
	event.Terminal, event.Result, event.NextAction = true, "failed", "check node logs"
	progress(event)
	if got, want := out.String(), "cp-1: kubeadm-init\ncp-1: kubeadm-init (failed)\nNext: check node logs\n"; got != want {
		t.Fatalf("progress = %q, want %q", got, want)
	}
}

func TestCommandOutputFailure(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--help"}, {"config", "schema"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			failure := errors.New("output closed")
			err := run(context.Background(), args, &failingOutput{err: failure}, io.Discard)
			if !errors.Is(err, failure) {
				t.Fatalf("command error = %v, want output failure", err)
			}
		})
	}
}

func TestOperationTextIncludesRecovery(t *testing.T) {
	client := &fakeKatlcAgentClient{operationStatus: &agentapi.OperationStatus{
		OperationId: "op-1", OperationKind: "host-upgrade", Terminal: true, Result: "failed",
		FailureReason: "image download failed", RecoveryRequired: true, NextAction: "check network and retry",
	}}
	installKatlcDial(t, nil, client)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"operations", "status", "op-1", "--endpoint", "192.0.2.1"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"failed", "image download failed", "Recovery required", "check network and retry"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q: %s", want, &out)
		}
	}
}

func TestExtensionStatusDetail(t *testing.T) {
	client := &fakeKatlcAgentClient{nodeStatus: &agentapi.NodeStatus{SystemExtensions: []*agentapi.SystemExtensionStatus{{
		Name: "storage", ActivationState: "active", Compatibility: "compatible",
		SubmittedReference: "registry.example/storage", OciManifestDigest: "artifact-digest",
		Files: []*agentapi.SystemExtensionFileStatus{{Path: "/etc/storage.conf", Sha256: "file-digest"}},
		Units: []*agentapi.SystemExtensionUnitStatus{
			{Name: "healthy.service", ActiveState: "active"},
			{Name: "failed.service", ActiveState: "failed", FailureDiagnostic: "disk is unavailable"},
		},
	}}}}
	installKatlcDial(t, nil, client)
	for _, tc := range []struct {
		name   string
		args   []string
		detail bool
	}{
		{name: "summary"},
		{name: "verbose", args: []string{"--verbose"}, detail: true},
		{name: "json", args: []string{"--output", "json"}, detail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			args := append([]string{"system-extension", "status", "--endpoint", "192.0.2.1"}, tc.args...)
			if err := run(context.Background(), args, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"storage", "compatible", "failed.service", "disk is unavailable"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing state or failure %q: %s", want, &out)
				}
			}
			for _, detail := range []string{"artifact-digest", "file-digest", "healthy.service"} {
				if strings.Contains(out.String(), detail) != tc.detail {
					t.Fatalf("detail %q present = %t, want %t: %s", detail, !tc.detail, tc.detail, &out)
				}
			}
			if tc.name == "json" && !json.Valid(out.Bytes()) {
				t.Fatalf("invalid JSON: %s", &out)
			}
		})
	}
}

func TestHostStatusProxyDetail(t *testing.T) {
	for _, tc := range []struct {
		name, kubernetes, state, canonical string
		verbose, detail                    bool
	}{
		{name: "healthy", kubernetes: "ready", state: "ready", canonical: "reachable"},
		{name: "verbose", kubernetes: "ready", state: "ready", canonical: "reachable", verbose: true, detail: true},
		{name: "unreachable", kubernetes: "ready", state: "ready", canonical: "unreachable", detail: true},
		{name: "failed", kubernetes: "ready", state: "no-eligible-backends", canonical: "unreachable", detail: true},
		{name: "before bootstrap", kubernetes: "not-configured", state: "no-eligible-backends", canonical: "unreachable"},
		{name: "failure before bootstrap", kubernetes: "not-configured", state: "failed", canonical: "unreachable", detail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := hostStatusReport{Node: "cp-1", Health: "OK",
				Kubernetes: &kubernetesStatusReport{State: tc.kubernetes},
				APIProxy:   &apiProxyStatusReport{State: tc.state, CanonicalState: tc.canonical},
			}
			var out bytes.Buffer
			if err := writeHostStatus(&out, "text", report, tc.verbose); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "API PROXY") != tc.detail {
				t.Fatalf("unexpected proxy detail: %s", &out)
			}
			if !strings.Contains(out.String(), tc.kubernetes) {
				t.Fatalf("Kubernetes state hidden: %s", &out)
			}
		})
	}
}

func TestTableRows(t *testing.T) {
	var out bytes.Buffer
	table := newTable(&out)
	table.row("NODE", "STATE")
	table.row("a", "OK")
	table.row("long-node", "failed")
	table.row("", "detail")
	if err := table.flush(); err != nil {
		t.Fatal(err)
	}

	want := "NODE       STATE\na          OK\nlong-node  failed\n           detail\n"
	if out.String() != want {
		t.Fatalf("table = %q, want %q", out.String(), want)
	}
}

type failingOutput struct {
	err    error
	writes int
}

func (w *failingOutput) Write([]byte) (int, error) { w.writes++; return 0, w.err }

func TestTableWriteErrors(t *testing.T) {
	for _, duringRow := range []bool{false, true} {
		name := "flush"
		if duringRow {
			name = "row"
		}
		t.Run(name, func(t *testing.T) {
			failure := errors.New("output closed")
			out := &failingOutput{err: failure}
			table := newTable(out)
			value := "node"
			if duringRow {
				value += "\f"
			}
			table.row(value)
			if duringRow && out.writes == 0 {
				t.Fatal("fixture did not exercise a write before flush")
			}
			table.row("another node")
			if err := table.flush(); !errors.Is(err, failure) {
				t.Fatalf("flush = %v", err)
			}
			if err := table.flush(); !errors.Is(err, failure) {
				t.Fatalf("repeated flush lost output failure: %v", err)
			}
			if out.writes != 1 {
				t.Fatalf("attempted %d writes after an output failure", out.writes)
			}
		})
	}
}

func TestProtoJSON(t *testing.T) {
	var out bytes.Buffer
	if err := writeProtoJSON(&out, "generation", &agentapi.GenerationMutationResult{GenerationId: "g1"}); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"generationId": "g1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON = %#v, want %#v", got, want)
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatal("JSON lacks final newline")
	}
}

func TestProtoJSONErrors(t *testing.T) {
	t.Run("marshal", func(t *testing.T) {
		var out bytes.Buffer
		err := writeProtoJSON(&out, "generation", &agentapi.GenerationMutationResult{GenerationId: string([]byte{0xff})})
		if err == nil || !strings.Contains(err.Error(), "marshal generation") {
			t.Fatalf("error = %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("partial JSON = %q", out.String())
		}
	})
	t.Run("write", func(t *testing.T) {
		failure := errors.New("output closed")
		err := writeProtoJSON(&failingOutput{err: failure}, "generation", &agentapi.GenerationMutationResult{GenerationId: "g1"})
		if !errors.Is(err, failure) {
			t.Fatalf("error = %v", err)
		}
	})
}
