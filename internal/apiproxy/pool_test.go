package apiproxy

import (
	"errors"
	"testing"
	"time"
)

func TestPoolPrefersEligibleLocalBackend(t *testing.T) {
	pool := newPool([]Backend{
		{Name: "cp-1", Address: "10.20.0.11:6443", Local: true},
		{Name: "cp-2", Address: "10.20.0.12:6443"},
	})
	checked := time.Now()
	pool.update("cp-1", true, "ready", checked)
	pool.update("cp-2", true, "ready", checked)
	backend, err := pool.selectBackend(nil)
	if err != nil {
		t.Fatalf("selectBackend() error = %v", err)
	}
	if backend.Name != "cp-1" {
		t.Fatalf("selected backend = %q, want cp-1", backend.Name)
	}
	pool.update("cp-1", false, "connection refused", checked)
	backend, err = pool.selectBackend(nil)
	if err != nil {
		t.Fatalf("selectBackend() peer error = %v", err)
	}
	if backend.Name != "cp-2" {
		t.Fatalf("selected backend = %q, want cp-2", backend.Name)
	}
}

func TestPoolSelectsPeersRoundRobinAndHonorsExclusion(t *testing.T) {
	pool := newPool([]Backend{
		{Name: "cp-1", Address: "10.20.0.11:6443"},
		{Name: "cp-2", Address: "10.20.0.12:6443"},
	})
	pool.update("cp-1", true, "ready", time.Now())
	pool.update("cp-2", true, "ready", time.Now())
	first, _ := pool.selectBackend(nil)
	second, _ := pool.selectBackend(nil)
	if first.Name == second.Name {
		t.Fatalf("round-robin selections = %q, %q", first.Name, second.Name)
	}
	excluded := map[string]struct{}{first.Name: {}}
	selected, err := pool.selectBackend(excluded)
	if err != nil {
		t.Fatalf("selectBackend(excluded) error = %v", err)
	}
	if selected.Name == first.Name {
		t.Fatalf("selected excluded backend %q", selected.Name)
	}
}

func TestPoolStartsWithdrawn(t *testing.T) {
	pool := newPool([]Backend{{Name: "cp-1", Address: "10.20.0.11:6443", Local: true}})
	if _, err := pool.selectBackend(nil); !errors.Is(err, ErrNoBackend) {
		t.Fatalf("selectBackend() error = %v, want ErrNoBackend", err)
	}
	status := pool.snapshot()
	if len(status) != 1 || status[0].Eligible || status[0].Reason != "not checked" {
		t.Fatalf("initial status = %#v", status)
	}
}
