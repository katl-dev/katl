package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/bgpapivip"
)

func TestWithdrawDisablesOnlyRouteSourceWhenBirdResponds(t *testing.T) {
	bird := &fakeBirdClient{}
	runner := &fakeCommandRunner{}
	if err := withdrawWith(context.Background(), bird, runner); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bird.advertisements, []bool{false}) {
		t.Fatalf("advertisements = %#v", bird.advertisements)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("fallback calls = %#v", runner.calls)
	}
}

func TestWithdrawStopsDedicatedBirdWhenRouteDisableCannotBeConfirmed(t *testing.T) {
	bird := &fakeBirdClient{setErr: errors.New("control socket unavailable")}
	runner := &fakeCommandRunner{}
	if err := withdrawWith(context.Background(), bird, runner); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"systemctl", "stop", "katl-app-bird.service"}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("fallback calls = %#v, want %#v", runner.calls, want)
	}
}

func TestWithdrawFailsWhenNeitherRouteNorDaemonCanBeStopped(t *testing.T) {
	bird := &fakeBirdClient{setErr: errors.New("control socket unavailable")}
	runner := &fakeCommandRunner{err: errors.New("systemctl failed"), output: []byte("access denied")}
	err := withdrawWith(context.Background(), bird, runner)
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("withdrawWith() error = %v", err)
	}
}

func TestControllerErrorFailsClosedBeforeSystemdRestart(t *testing.T) {
	bird := &fakeBirdClient{}
	runner := &fakeCommandRunner{}
	runErr := errors.New("routing status unavailable")

	err := failClosed(runErr, bird, runner)
	if !errors.Is(err, runErr) {
		t.Fatalf("failClosed() error = %v", err)
	}
	if !reflect.DeepEqual(bird.advertisements, []bool{false}) {
		t.Fatalf("advertisements = %#v", bird.advertisements)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("fallback calls = %#v", runner.calls)
	}
}

type fakeBirdClient struct {
	setErr         error
	advertisements []bool
}

func (b *fakeBirdClient) Status(context.Context) (bgpapivip.BirdRuntimeStatus, error) {
	return bgpapivip.BirdRuntimeStatus{}, nil
}

func (b *fakeBirdClient) SetAdvertisement(_ context.Context, enabled bool) error {
	b.advertisements = append(b.advertisements, enabled)
	return b.setErr
}

type fakeCommandRunner struct {
	calls  [][]string
	output []byte
	err    error
}

func (r *fakeCommandRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.output, r.err
}
