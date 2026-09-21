package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc"
)

type generationClient struct {
	*fakeKatlcAgentClient
	request *agentapi.GenerationMutationRequest
	action  string
}

func (c *generationClient) SelectGeneration(_ context.Context, r *agentapi.GenerationMutationRequest, _ ...grpc.CallOption) (*agentapi.GenerationMutationResult, error) {
	c.request = r
	c.action = "select"
	return &agentapi.GenerationMutationResult{GenerationId: r.GenerationId, OneShot: r.OneShot}, nil
}
func (c *generationClient) RemoveGeneration(_ context.Context, r *agentapi.GenerationMutationRequest, _ ...grpc.CallOption) (*agentapi.GenerationMutationResult, error) {
	c.request = r
	c.action = "remove"
	return &agentapi.GenerationMutationResult{GenerationId: r.GenerationId}, nil
}
func (c *generationClient) ListGenerations(context.Context, *agentapi.ListGenerationsRequest, ...grpc.CallOption) (*agentapi.ListGenerationsResponse, error) {
	return &agentapi.ListGenerationsResponse{DefaultGenerationId: "current", NextBootGenerationId: "previous", OneShot: true, KeepLast: 5, MaxAge: "720h", Generations: []*agentapi.Generation{{GenerationId: "previous", RuntimeVersion: "1", RootSlot: "root-b", ProtectedBy: []string{"slot rollback"}}, {GenerationId: "current", RuntimeVersion: "2", RootSlot: "root-a", ProtectedBy: []string{"active", "default"}}}}, nil
}

func TestGenerationCommands(t *testing.T) {
	for _, test := range []struct {
		args    []string
		want    string
		action  string
		oneShot bool
	}{
		{[]string{"list"}, "slot rollback", "", false},
		{[]string{"select", "previous"}, "persistent default", "select", false},
		{[]string{"select", "previous", "--one-shot"}, "one-shot boot", "select", true},
		{[]string{"remove", "previous"}, "Generation previous removed", "remove", false},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			fake := &generationClient{fakeKatlcAgentClient: healthyHostClient("machine-a", "agent-a", "current")}
			installKatlcDial(t, nil, fake)
			args := append([]string{"node", "generations"}, test.args...)
			args = append(args, "--node", "node-a", "--endpoint", "node-a.test:9443")
			var out, diagnostics bytes.Buffer
			if err := run(context.Background(), args, &out, &diagnostics); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), test.want) {
				t.Fatalf("output %s", out.String())
			}
			if test.action != "" {
				if fake.action != test.action || fake.request.GenerationId != "previous" || fake.request.OneShot != test.oneShot || fake.request.ExpectedMachineId != "machine-a" || fake.request.ExpectedCurrentGenerationId != "current" {
					t.Fatalf("request %+v", fake.request)
				}
			}
		})
	}
}

func TestGenerationFailureReleasesSession(t *testing.T) {
	for _, mode := range []string{"status", "identity"} {
		t.Run(mode, func(t *testing.T) {
			client := &generationClient{fakeKatlcAgentClient: healthyHostClient("machine-a", "agent-a", "current")}
			if mode == "status" {
				client.nodeStatusErr = errors.New("status unavailable")
			} else {
				client.nodeStatus.InventoryNodeName = "another-node"
			}
			oldDial := dialKatlcAgent
			t.Cleanup(func() { dialKatlcAgent = oldDial })
			var requestCtx context.Context
			closed := 0
			dialKatlcAgent = func(ctx context.Context, _ string) (katlcAgentConnection, error) {
				requestCtx = ctx
				return katlcAgentConnection{Client: client, Close: func() error { closed++; return nil }}, nil
			}

			err := run(context.Background(), []string{"node", "generations", "select", "previous", "--node", "node-a", "--endpoint", "node.test"}, io.Discard, io.Discard)
			if err == nil {
				t.Fatal("failed preflight accepted a selection")
			}
			if client.request != nil {
				t.Fatalf("failed preflight sent mutation: %+v", client.request)
			}
			if closed != 1 || requestCtx == nil || !errors.Is(requestCtx.Err(), context.Canceled) {
				t.Fatalf("session not released: closes=%d, context=%v", closed, requestCtx)
			}
		})
	}
}
