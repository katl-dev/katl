package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
)

func TestWithdrawReleasesVIPWithoutStoppingBird(t *testing.T) {
	owner := &fakeVIPOwner{owned: true}
	if err := withdrawWith(context.Background(), owner, bgpapivip.Config{}); err != nil {
		t.Fatal(err)
	}
	if owner.owned {
		t.Fatal("withdraw retained local VIP ownership")
	}
}

func TestWithdrawReportsVIPReleaseFailure(t *testing.T) {
	owner := &fakeVIPOwner{owned: true, err: errors.New("netlink unavailable")}
	err := withdrawWith(context.Background(), owner, bgpapivip.Config{})
	if err == nil || !strings.Contains(err.Error(), "netlink unavailable") {
		t.Fatalf("withdrawWith() error = %v", err)
	}
	if !owner.owned {
		t.Fatal("failed release changed local VIP ownership")
	}
}

func TestControllerErrorFailsClosedBeforeSystemdRestart(t *testing.T) {
	owner := &fakeVIPOwner{owned: true}
	runErr := errors.New("routing status unavailable")

	err := failClosed(runErr, owner, bgpapivip.Config{})
	if !errors.Is(err, runErr) {
		t.Fatalf("failClosed() error = %v", err)
	}
	if owner.owned {
		t.Fatal("failClosed retained local VIP ownership")
	}
}

type fakeVIPOwner struct {
	owned bool
	err   error
}

func (o *fakeVIPOwner) Owned(context.Context, bgpapivip.Config) (bool, error) {
	return o.owned, o.err
}

func (o *fakeVIPOwner) SetOwned(_ context.Context, _ bgpapivip.Config, owned bool) error {
	if o.err != nil {
		return o.err
	}
	o.owned = owned
	return nil
}
