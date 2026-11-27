package vmtest

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/retry"
)

// TestInstalledRuntimeLinkRecovery checks that the link-recovery service runs
// on a known-good boot and that its one reconfiguration of a required link
// succeeds under the shipped sandbox. The VM cannot reproduce on demand
// the state in which systemd-networkd reports no carrier while sysfs reports
// carrier up, so a test-only networkctl reports it for the real management
// link, whose sysfs carrier is up.
func TestInstalledRuntimeLinkRecovery(t *testing.T) {
	if !DefaultOptions().Enabled {
		t.Skip("run with scripts/vmtest-run")
	}
	const scenario = "installed-runtime-link-recovery"
	run, ok := installedRuntimeWorldRunFor(t, scenario, NodeSpec{Name: "cp-1", Role: ControlPlane})
	if !ok {
		t.Fatal("run this test with a vmtest world")
	}
	result, err := run.Runner.Plan(Scenario{Name: scenario})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	katlctl := configApplyKatlctl(BuildKatlctl(t, ctx, repoRoot(t)), result)

	run.Config.VM.Timeout = 10 * time.Minute
	run.Config.VM.PreserveNVRAM = true
	node, err := StartInstalledRuntimeNode(ctx, result, InstalledRuntimeNodeConfig{
		Name: run.Node.Name, Runtime: run.Config,
	}, VMRunner{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			_ = node.StopFailure("link recovery failed")
		} else if err := node.Stop(); err != nil && err != context.Canceled {
			t.Errorf("stop installed node: %v", err)
		}
	}()

	client, err := DialAgent(ctx, node.VSock.GuestCID, node.VSock.Port, node.Result.Artifacts.VSockTranscript)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	guest := NewGuestControl(node.Result, client)
	current := currentGenerationFromGuest(t, ctx, guest)
	waitGenerationPromotion(t, ctx, guest, current)
	enrollConfigApplyNode(t, ctx, result, katlctl, katlcEndpoint(t, node, run.Node.Address))

	const service = "katl-link-recovery.service"
	if state := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "link-recovery-active", "systemctl", "is-active", service)); state != "active" {
		t.Fatalf("%s is %q on a known-good boot, want active", service, state)
	}

	// Present the management link as networkd without carrier, with every
	// other field as networkd reports it now.
	link := defaultRouteLink(t, ctx, guest)
	var state struct {
		Interfaces []map[string]any `json:"Interfaces"`
	}
	if err := json.Unmarshal([]byte(guestCommandOutput(t, ctx, guest, "networkd-state", "networkctl", "list", "--json=short", "--no-pager")), &state); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range state.Interfaces {
		if entry["Name"] == link {
			if entry["RequiredForOnline"] != true {
				t.Fatalf("management link %s is not required for online: %v", link, entry)
			}
			entry["OnlineState"], entry["OperationalState"], entry["CarrierState"] = "offline", "no-carrier", "no-carrier"
			found = true
		}
	}
	if !found {
		t.Fatalf("networkd does not report management link %s", link)
	}
	fixture, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	installFile := func(name, path, mode string, content []byte) {
		t.Helper()
		staged := "/var/lib/katl/test-artifacts/" + name
		writeGuestFile(t, ctx, guest, staged, content, 0o644)
		guestCommand(t, ctx, guest, "install-"+name, "install", "-D", "-m", mode, staged, path)
	}
	// The service finds networkctl through PATH. The test's networkctl comes
	// first: it logs the calling process and its arguments for every call,
	// prints the recorded state for 'networkctl list', and runs the real
	// networkctl for everything else, so the reconfiguration runs under the
	// shipped sandbox.
	const fixtureDir = "/var/lib/katl/test-artifacts/link-recovery-vmtest"
	const callLog = fixtureDir + "/calls"
	installFile("link-recovery-networkd-state.json", fixtureDir+"/networkd-state.json", "0644", fixture)
	installFile("link-recovery-networkctl", fixtureDir+"/bin/networkctl", "0755", []byte(`#!/usr/bin/sh
echo "$PPID $*" >>`+callLog+`
if [ "${1:-}" = list ]; then
	exec /usr/bin/cat `+fixtureDir+`/networkd-state.json
fi
exec /usr/bin/networkctl "$@"
`))
	installFile("link-recovery-path.conf", "/run/systemd/system/"+service+".d/90-vmtest-networkctl.conf", "0644", []byte(
		"[Service]\nEnvironment=PATH="+fixtureDir+"/bin:/usr/bin:/usr/sbin\nReadWritePaths="+fixtureDir+"\n"))
	installFile("management-carrier-timer.conf", "/run/systemd/system/katl-deadline-management-carrier@.timer.d/90-vmtest-disable.conf",
		"0644", []byte(DeadlineTimerDisabled))
	guestCommand(t, ctx, guest, "link-recovery-daemon-reload", "systemctl", "daemon-reload")

	// calls returns the arguments of each networkctl call the given process
	// made.
	calls := func(pid string) []string {
		t.Helper()
		var args []string
		for _, line := range strings.Split(readGuestFile(t, ctx, guest, callLog), "\n") {
			if caller, rest, _ := strings.Cut(line, " "); caller == pid {
				args = append(args, rest)
			}
		}
		return args
	}
	// Each run of the service is read through its own invocation's journal
	// and identified by its main process.
	restart := func(phase string) (invocation, pid string) {
		t.Helper()
		guestCommand(t, ctx, guest, phase+"-restart", "systemctl", "restart", service)
		show := func(property string) string {
			return strings.TrimSpace(guestCommandOutput(t, ctx, guest, phase+"-"+property, "systemctl", "show", "--property="+property, "--value", service))
		}
		invocation, pid = show("InvocationID"), show("MainPID")
		if invocation == "" || pid == "" || pid == "0" {
			t.Fatalf("%s: %s has invocation %q and main process %q", phase, service, invocation, pid)
		}
		return invocation, pid
	}
	journal := func(phase, invocation string) string {
		t.Helper()
		return guestCommandOutput(t, ctx, guest, phase, "journalctl", "-b", "_SYSTEMD_INVOCATION_ID="+invocation, "--no-pager", "-q", "-o", "cat")
	}
	await := func(phase, invocation string, timeout time.Duration, done func(string) bool) string {
		t.Helper()
		waitCtx, stop := context.WithTimeout(ctx, timeout)
		defer stop()
		backoff := retry.NewBackoff(500*time.Millisecond, 2*time.Second)
		text, err := retry.Await(waitCtx, &backoff, func(ctx context.Context) (string, bool, error) {
			text := journal(phase, invocation)
			return text, done(text), nil
		}, nil)
		if err != nil {
			t.Fatalf("%s: %v\n%s", phase, err, text)
		}
		return text
	}
	const deadlines = "/run/katl/deadlines"
	deadlineMarkers := func(phase string) []string {
		t.Helper()
		return strings.Fields(guestCommandOutput(t, ctx, guest, phase, "find", deadlines, "-mindepth", "1", "-maxdepth", "1", "-printf", "%f\n"))
	}
	graceStarted := "required link " + link + ": systemd-networkd reports no carrier while sysfs reports carrier up; reconfiguring once if this persists for 20s"

	markersBefore := deadlineMarkers("deadlines-before")
	first, firstPID := restart("fixture")
	await("grace-started", first, time.Minute, func(text string) bool { return strings.Contains(text, graceStarted) })
	// The grace period's marker is the deadline instance the service armed.
	var armed []string
	for _, marker := range deadlineMarkers("deadlines-armed") {
		if !slices.Contains(markersBefore, marker) {
			armed = append(armed, marker)
		}
	}
	if len(armed) != 1 {
		t.Fatalf("%s armed deadline instances %q after starting a grace period, want one", service, armed)
	}
	if record, err := guest.RunCommand(ctx, GuestCommandRequest{
		Name: "no-record-during-grace", Argv: []string{"test", "-e", "/run/katl/link-recovery/" + link}, AllowFailure: true,
	}); err != nil || record.ExitStatus == 0 {
		t.Fatalf("%s reconfigured %s before its grace period elapsed (%v)", service, link, err)
	}

	guestCommand(t, ctx, guest, "expire-grace", "systemctl", "start", "katl-deadline-expire@"+armed[0]+".service")
	reconfigured := "Required link " + link + " was reconfigured once in this boot"
	events := await("reconfigured", first, time.Minute, func(text string) bool {
		return strings.Contains(text, "Required link "+link+" ")
	})
	if strings.Count(events, reconfigured) != 1 || strings.Contains(events, "could not be reconfigured") || strings.Contains(events, "not reconfiguring") {
		t.Fatalf("%s did not report one successful reconfiguration of %s:\n%s", service, link, events)
	}

	// The reconfiguration ran under the shipped sandbox, which leaves the
	// service without effective capabilities.
	capEff := ""
	for _, line := range strings.Split(readGuestFile(t, ctx, guest, "/proc/"+firstPID+"/status"), "\n") {
		if value, ok := strings.CutPrefix(line, "CapEff:"); ok {
			capEff = strings.TrimSpace(value)
		}
	}
	if capEff == "" || strings.Trim(capEff, "0") != "" {
		t.Fatalf("%s (pid %s) has effective capabilities %q, want none", service, firstPID, capEff)
	}

	records := strings.Fields(guestCommandOutput(t, ctx, guest, "link-recovery-records", "find", "/run/katl/link-recovery", "-mindepth", "1", "-maxdepth", "1"))
	if len(records) != 1 || records[0] != "/run/katl/link-recovery/"+link {
		t.Fatalf("link recovery records = %q, want only %s", records, link)
	}
	recordPath := records[0]
	before := readGuestFile(t, ctx, guest, recordPath)
	var record struct {
		Link, Outcome, Error string
	}
	if err := json.Unmarshal([]byte(before), &record); err != nil || record.Link != link || record.Outcome != "reconfigured" || record.Error != "" {
		t.Fatalf("link recovery record = %s (%v), want a successful reconfiguration of %s", before, err, link)
	}

	// The reconfiguration may briefly interrupt the management address.
	type nodeStatus struct {
		Health               string `json:"health"`
		BootHealthDiagnostic string `json:"bootHealthDiagnostic"`
	}
	statusCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	backoff := retry.NewBackoff(500*time.Millisecond, 2*time.Second)
	status, err := retry.Await(statusCtx, &backoff, func(ctx context.Context) (nodeStatus, bool, error) {
		output, err := runKatlctlOutcome(t, ctx, result, katlctl, "link-recovery-status", "node", "status", node.Name, "--output", "json")
		if err != nil {
			return nodeStatus{}, false, nil
		}
		var got nodeStatus
		err = json.Unmarshal(output, &got)
		return got, got.Health == "OK" && strings.Contains(got.BootHealthDiagnostic, reconfigured), err
	}, nil)
	if err != nil {
		t.Fatalf("node status = %+v, want healthy and reporting the reconfiguration of %s: %v", status, link, err)
	}

	// A restarted service reads the boot's record and makes no second attempt.
	second, secondPID := restart("restarted")
	// The service polls at least every five seconds; this window spans
	// several observations of the disagreement.
	observe, stopObserving := context.WithTimeout(ctx, 15*time.Second)
	<-observe.Done()
	stopObserving()
	if n := len(slices.DeleteFunc(calls(secondPID), func(args string) bool { return !strings.HasPrefix(args, "list ") })); n < 2 {
		t.Fatalf("restarted %s (pid %s) observed the recorded state %d times, want several", service, secondPID, n)
	}
	if text := journal("restarted-events", second); strings.Contains(strings.ToLower(text), "required link "+link) {
		t.Fatalf("restarted %s acted on %s again:\n%s", service, link, text)
	}
	if active := strings.TrimSpace(guestCommandOutput(t, ctx, guest, "restarted-active", "systemctl", "is-active", service)); active != "active" {
		t.Fatalf("restarted %s is %q, want active", service, active)
	}
	// Exactly one reconfiguration ran in this boot, from the first process.
	reconfigure := firstPID + " reconfigure " + link
	var reconfigurations []string
	for _, line := range strings.Split(readGuestFile(t, ctx, guest, callLog), "\n") {
		if _, args, _ := strings.Cut(line, " "); strings.HasPrefix(args, "reconfigure") {
			reconfigurations = append(reconfigurations, line)
		}
	}
	if len(reconfigurations) != 1 || reconfigurations[0] != reconfigure {
		t.Fatalf("networkctl reconfigurations = %q, want only %q", reconfigurations, reconfigure)
	}
	if after := readGuestFile(t, ctx, guest, recordPath); after != before {
		t.Fatalf("link recovery record changed across restart:\n%s\n%s", before, after)
	}
	if records := strings.Fields(guestCommandOutput(t, ctx, guest, "restarted-records", "find", "/run/katl/link-recovery", "-mindepth", "1", "-maxdepth", "1")); len(records) != 1 {
		t.Fatalf("link recovery records after restart = %q, want one", records)
	}

	if err := run.Scenario.WriteResult(WorldStatusPassed, ""); err != nil {
		t.Fatal(err)
	}
}
