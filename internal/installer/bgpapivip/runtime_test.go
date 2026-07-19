package bgpapivip

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestCommandBirdClientControlsOnlyAPIRoute(t *testing.T) {
	runner := &recordingCommandRunner{outputs: [][]byte{
		[]byte("BIRD ready.\n"),
		[]byte("BIRD ready.\n"),
	}}
	client := CommandBirdClient{Runner: runner}
	if err := client.SetAdvertisement(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := client.SetAdvertisement(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"birdc", "-s", BirdControlSocketPath, "disable", "katl_api"},
		{"birdc", "-s", BirdControlSocketPath, "enable", "katl_api"},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, want)
	}
}

func TestCommandBirdClientReportsBoundedPeerState(t *testing.T) {
	config := minimalConfig()
	config, err := Normalize(config)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingCommandRunner{outputs: [][]byte{[]byte(`Name Proto Table State Since Info
katl_fabric_router_a BGP katl_fabric up 12:00:00 Established
`)}}
	client := CommandBirdClient{Runner: runner, Config: config}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.ControlSocketReady || len(status.Peers) != 1 || status.Peers[0].SessionState != "established" {
		t.Fatalf("status = %#v", status)
	}
}

func TestCommandBirdClientFailsClosedWhenControlSocketIsUnavailable(t *testing.T) {
	runner := &recordingCommandRunner{err: errors.New("exit status 1")}
	client := CommandBirdClient{Runner: runner}
	status, err := client.Status(context.Background())
	if err == nil || status.ControlSocketReady || status.ProcessActive {
		t.Fatalf("status = %#v, error = %v", status, err)
	}
}

type recordingCommandRunner struct {
	outputs  [][]byte
	err      error
	commands [][]string
}

func (r *recordingCommandRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, append([]string{name}, args...))
	var output []byte
	if len(r.outputs) > 0 {
		output = r.outputs[0]
		r.outputs = r.outputs[1:]
	}
	return output, r.err
}
