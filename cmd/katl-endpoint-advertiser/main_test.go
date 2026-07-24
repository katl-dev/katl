package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
)

func TestWithdrawReleasesVIPWithoutStoppingBird(t *testing.T) {
	owner := &fakeVIPOwner{owned: true}
	runner := &fakeCommandRunner{}
	if err := withdrawWith(context.Background(), owner, bgpapivip.Config{}, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("fallback calls = %#v", runner.calls)
	}
	if owner.owned {
		t.Fatal("withdraw retained local VIP ownership")
	}
}

func TestWithdrawStopsDedicatedBirdWhenVIPCannotBeReleased(t *testing.T) {
	owner := &fakeVIPOwner{owned: true, err: errors.New("netlink unavailable")}
	runner := &fakeCommandRunner{}
	err := withdrawWith(context.Background(), owner, bgpapivip.Config{}, runner)
	if err == nil || !strings.Contains(err.Error(), "netlink unavailable") {
		t.Fatalf("withdrawWith() error = %v", err)
	}
	want := [][]string{{"systemctl", "stop", "katl-app-bird.service"}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("fallback calls = %#v, want %#v", runner.calls, want)
	}
}

func TestWithdrawFailsWhenNeitherRouteNorDaemonCanBeStopped(t *testing.T) {
	owner := &fakeVIPOwner{owned: true, err: errors.New("netlink unavailable")}
	runner := &fakeCommandRunner{err: errors.New("systemctl failed"), output: []byte("access denied")}
	err := withdrawWith(context.Background(), owner, bgpapivip.Config{}, runner)
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("withdrawWith() error = %v", err)
	}
}

func TestControllerErrorFailsClosedBeforeSystemdRestart(t *testing.T) {
	owner := &fakeVIPOwner{owned: true}
	runner := &fakeCommandRunner{}
	runErr := errors.New("routing status unavailable")

	err := failClosed(runErr, owner, bgpapivip.Config{}, runner)
	if !errors.Is(err, runErr) {
		t.Fatalf("failClosed() error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("fallback calls = %#v", runner.calls)
	}
	if owner.owned {
		t.Fatal("failClosed retained local VIP ownership")
	}
}

type fakeCommandRunner struct {
	calls  [][]string
	output []byte
	err    error
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

func (r *fakeCommandRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.output, r.err
}
