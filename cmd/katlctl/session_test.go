package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
)

func TestManagementSessionLifetime(t *testing.T) {
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	closed := 0
	var requestCtx context.Context
	dialKatlcAgent = func(ctx context.Context, endpoint string) (katlcAgentConnection, error) {
		requestCtx = ctx
		if endpoint != "node.test:9443" {
			t.Fatalf("endpoint = %q", endpoint)
		}
		identity, ok := ctx.Value(managementDialIdentityKey{}).(managementDialIdentity)
		if !ok || identity.nodeName != "node-a" || identity.credentials == nil || identity.credentials.Authentication != managementidentity.TrustedNetwork {
			t.Fatalf("dial identity = %+v", identity)
		}
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.After(time.Now()) || deadline.After(time.Now().Add(time.Minute)) {
			t.Fatalf("request deadline = %v, present = %v", deadline, ok)
		}
		return katlcAgentConnection{Client: healthyHostClient("machine-a", "agent-a", "current"), Close: func() error { closed++; return nil }}, nil
	}

	session, err := openManagementSession(parent, managementTargetOptions{nodeName: "node-a", endpoint: "node.test"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	session.close()
	session.close()

	if closed != 1 {
		t.Fatalf("connection closed %d times", closed)
	}
	if !errors.Is(requestCtx.Err(), context.Canceled) {
		t.Fatalf("request remains live: %v", requestCtx.Err())
	}
	if parent.Err() != nil {
		t.Fatalf("session canceled parent: %v", parent.Err())
	}
}

func TestManagementSessionDialFailure(t *testing.T) {
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	failure := errors.New("connection refused")
	var requestCtx context.Context
	dialKatlcAgent = func(ctx context.Context, _ string) (katlcAgentConnection, error) {
		requestCtx = ctx
		return katlcAgentConnection{}, failure
	}

	session, err := openManagementSession(context.Background(), managementTargetOptions{nodeName: "node-a", endpoint: "node.test"}, time.Minute)
	if session != nil || !errors.Is(err, failure) {
		t.Fatalf("session = %+v, error = %v", session, err)
	}
	if !errors.Is(requestCtx.Err(), context.Canceled) {
		t.Fatalf("failed dial left request live: %v", requestCtx.Err())
	}
}

func TestManagementSessionRejectsTimeout(t *testing.T) {
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	dialKatlcAgent = func(context.Context, string) (katlcAgentConnection, error) {
		t.Fatal("invalid request reached dial")
		return katlcAgentConnection{}, nil
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		if _, err := openManagementSession(context.Background(), managementTargetOptions{endpoint: "node.test"}, timeout); err == nil {
			t.Fatalf("accepted timeout %s", timeout)
		}
	}
}

func TestManagementSessionParentCancellation(t *testing.T) {
	oldDial := dialKatlcAgent
	t.Cleanup(func() { dialKatlcAgent = oldDial })
	var requestCtx context.Context
	dialKatlcAgent = func(ctx context.Context, _ string) (katlcAgentConnection, error) {
		requestCtx = ctx
		return katlcAgentConnection{Close: func() error { return nil }}, nil
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, err := openManagementSession(parent, managementTargetOptions{endpoint: "node.test"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()

	cancel()
	if !errors.Is(requestCtx.Err(), context.Canceled) {
		t.Fatalf("parent cancellation did not reach request: %v", requestCtx.Err())
	}
}
