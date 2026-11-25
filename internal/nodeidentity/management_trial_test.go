package nodeidentity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
)

type fakeTrialClock struct {
	mu        sync.Mutex
	bootID    string
	sinceBoot time.Duration
	now       time.Time
}

func (c *fakeTrialClock) BootID() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bootID, nil
}

func (c *fakeTrialClock) SinceBoot() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sinceBoot, nil
}

func (c *fakeTrialClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeTrialClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sinceBoot += d
	c.now = c.now.Add(d)
}

func (c *fakeTrialClock) reboot() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bootID = c.bootID + "-next"
	c.sinceBoot = time.Minute
}

type trialNode struct {
	root        string
	request     ManagementTransition
	authority   string
	clock       *fakeTrialClock
	armedTrials []ManagementTrial
}

func newTrialNode(t *testing.T) *trialNode {
	t.Helper()
	root := t.TempDir()
	machineID, err := WriteMachineID(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := WriteEnrollment(root, "cp-1", machineID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteManagementIdentity(root, "cp-1", managementidentity.NodeCredentials{Authentication: managementidentity.TrustedNetwork}); err != nil {
		t.Fatal(err)
	}
	replacement := testManagementIdentity(t, "cp-1")
	authority, err := managementidentity.CAFingerprint(replacement.CACertificate)
	if err != nil {
		t.Fatal(err)
	}
	return &trialNode{
		root: root, authority: authority,
		clock: &fakeTrialClock{bootID: "boot-a", sinceBoot: time.Hour, now: time.Now().UTC()},
		request: ManagementTransition{
			NodeName: "cp-1", ExpectedEnrollmentID: enrollment.ID, ExpectedMachineID: machineID,
			ExpectedAuthentication: managementidentity.TrustedNetwork, Replacement: replacement, Window: time.Minute,
		},
	}
}

func (n *trialNode) begin() (ManagementTrial, error) {
	return BeginManagementTrial(n.root, n.request, n.clock, func(trial ManagementTrial) error {
		n.armedTrials = append(n.armedTrials, trial)
		return nil
	})
}

func (n *trialNode) mustBegin(t *testing.T) ManagementTrial {
	t.Helper()
	trial, err := n.begin()
	if err != nil || trial.State != ManagementTrialLive {
		t.Fatalf("begin = %+v, %v", trial, err)
	}
	return trial
}

// assertInstalled checks the state a restarted agent would serve outside any
// trial: the mode file and, for mTLS, the credentials active selects. A
// trusted-network node ignores active, so only a revert must clear it.
func (n *trialNode) assertInstalled(t *testing.T, want managementidentity.Authentication) {
	t.Helper()
	mode, err := ManagementAuthentication(n.root)
	if err != nil || mode != want {
		t.Fatalf("installed mode = %q, %v; want %q", mode, err, want)
	}
	active, err := os.Readlink(filepath.Join(n.root, managementDirectory, "active"))
	switch want {
	case managementidentity.MutualTLS:
		if err != nil || active != filepath.Join("versions", n.authority) {
			t.Fatalf("active = %q, %v; want the replacement version", active, err)
		}
		caPath, certPath, keyPath, err := ManagementCredentialsPaths(n.root)
		if err != nil {
			t.Fatal(err)
		}
		for path, data := range map[string]string{caPath: n.request.Replacement.CACertificate, certPath: n.request.Replacement.ServerCertificate, keyPath: n.request.Replacement.ServerPrivateKey} {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatalf("installed %s is not the replacement: %v", filepath.Base(path), err)
			}
		}
	}
}

func (n *trialNode) assertActiveRestored(t *testing.T) {
	t.Helper()
	if active, err := os.Readlink(filepath.Join(n.root, managementDirectory, "active")); !os.IsNotExist(err) {
		t.Fatalf("active = %q, %v; want the trusted-network node's absent link", active, err)
	}
}

func (n *trialNode) recorded(t *testing.T) (ManagementTrial, bool) {
	t.Helper()
	trial, found, err := ReadManagementTrial(n.root)
	if err != nil {
		t.Fatal(err)
	}
	return trial, found
}

func TestManagementTrialBeginIsIdempotent(t *testing.T) {
	node := newTrialNode(t)
	first := node.mustBegin(t)
	node.clock.advance(10 * time.Second)
	again := node.mustBegin(t)

	if again.ID != first.ID || !again.Deadline.Equal(first.Deadline) {
		t.Fatalf("repeated begin started a new trial: %+v, first %+v", again, first)
	}
	if len(node.armedTrials) != 1 {
		t.Fatalf("revert armed %d times; want once for one trial", len(node.armedTrials))
	}
	node.assertInstalled(t, managementidentity.TrustedNetwork)
	if first.DeadlineSinceBoot != time.Hour+time.Minute {
		t.Fatalf("deadline since boot = %s; want start plus the window", first.DeadlineSinceBoot)
	}
}

func TestManagementTrialBeginRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *trialNode)
		want   string
	}{
		{name: "other node", change: func(t *testing.T, n *trialNode) { n.request.NodeName = "cp-2" }, want: "enrolled"},
		{name: "other enrollment", change: func(t *testing.T, n *trialNode) { n.request.ExpectedEnrollmentID = strings.Repeat("0", 32) }, want: "enrollment changed"},
		{name: "other machine", change: func(t *testing.T, n *trialNode) { n.request.ExpectedMachineID = strings.Repeat("0", 32) }, want: "machine ID"},
		{name: "unexpected mode", change: func(t *testing.T, n *trialNode) { n.request.ExpectedAuthentication = managementidentity.MutualTLS }, want: "uses trusted-network"},
		{name: "credentials for another node", change: func(t *testing.T, n *trialNode) { n.request.Replacement = testManagementIdentity(t, "cp-2") }, want: "validate replacement"},
		{name: "window too long", change: func(t *testing.T, n *trialNode) { n.request.Window = time.Hour }, want: "window"},
		{name: "second authority while live", change: func(t *testing.T, n *trialNode) {
			n.mustBegin(t)
			n.request.Replacement = testManagementIdentity(t, "cp-1")
		}, want: "in progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := newTrialNode(t)
			tc.change(t, node)
			before, hadTrial := node.recorded(t)

			if _, err := node.begin(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("begin error = %v; want %q", err, tc.want)
			}
			after, hasTrial := node.recorded(t)
			if hadTrial != hasTrial || after.ID != before.ID {
				t.Fatalf("refused begin changed the trial record: %+v -> %+v", before, after)
			}
			node.assertInstalled(t, managementidentity.TrustedNetwork)
		})
	}
}

func TestManagementTrialArmFailureRecordsNothing(t *testing.T) {
	node := newTrialNode(t)
	_, err := BeginManagementTrial(node.root, node.request, node.clock, func(ManagementTrial) error { return errors.New("systemd-run failed") })
	if err == nil {
		t.Fatal("begin succeeded without a scheduled revert")
	}
	if _, found := node.recorded(t); found {
		t.Fatal("a trial without a scheduled revert was recorded")
	}
	if trial, err := SettleManagementTrial(node.root, node.clock); err != nil || trial != nil {
		t.Fatalf("agent start would serve %+v, %v; want the installed mode", trial, err)
	}
}

func TestManagementTrialConfirmCommits(t *testing.T) {
	node := newTrialNode(t)
	node.mustBegin(t)
	if trial, err := SettleManagementTrial(node.root, node.clock); err != nil || trial == nil || trial.TargetAuthority != node.authority {
		t.Fatalf("restarted agent serves %+v, %v; want the live trial", trial, err)
	}

	for range 2 {
		result, err := ConfirmManagementTrial(node.root, node.authority, node.clock)
		if err != nil || result.State != ManagementTrialCommitted {
			t.Fatalf("confirm = %+v, %v", result, err)
		}
	}
	node.assertInstalled(t, managementidentity.MutualTLS)
	if _, found := node.recorded(t); found {
		t.Fatal("committed trial left its record")
	}
	if installed, err := InstalledManagementAuthority(node.root); err != nil || installed != node.authority {
		t.Fatalf("installed authority = %q, %v", installed, err)
	}

	// The deadline passing, the revert timer firing and a reboot all leave a
	// committed node alone.
	node.clock.advance(time.Hour)
	if _, reverted, err := RevertManagementTrial(node.root, "", "timer", node.clock); err != nil || reverted {
		t.Fatalf("revert after commit = %v, %v", reverted, err)
	}
	node.clock.reboot()
	if trial, err := SettleManagementTrial(node.root, node.clock); err != nil || trial != nil {
		t.Fatalf("agent start after commit = %+v, %v", trial, err)
	}
	node.assertInstalled(t, managementidentity.MutualTLS)
	again, err := node.begin()
	if err != nil || again.State != ManagementTrialCommitted {
		t.Fatalf("begin after commit = %+v, %v; want the committed result", again, err)
	}
}

func TestManagementTrialConfirmRefusesOtherAuthority(t *testing.T) {
	node := newTrialNode(t)
	node.mustBegin(t)
	other, err := managementidentity.CAFingerprint(testManagementIdentity(t, "cp-1").CACertificate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ConfirmManagementTrial(node.root, other, node.clock); err == nil || !strings.Contains(err.Error(), "targets authority") {
		t.Fatalf("confirm for another authority = %v", err)
	}
	node.assertInstalled(t, managementidentity.TrustedNetwork)
	if trial, found := node.recorded(t); !found || trial.State != ManagementTrialLive {
		t.Fatalf("refused confirm changed the trial: %+v", trial)
	}
}

func TestManagementTrialReverts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revert func(*testing.T, *trialNode, ManagementTrial)
		reason string
	}{
		{name: "timer", revert: func(t *testing.T, n *trialNode, trial ManagementTrial) {
			if _, reverted, err := RevertManagementTrial(n.root, trial.ID, "the transition was not confirmed before its deadline", n.clock); err != nil || !reverted {
				t.Fatalf("revert = %v, %v", reverted, err)
			}
		}, reason: "deadline"},
		{name: "agent start after deadline", revert: func(t *testing.T, n *trialNode, _ ManagementTrial) {
			n.clock.advance(time.Minute)
			if trial, err := SettleManagementTrial(n.root, n.clock); err != nil || trial != nil {
				t.Fatalf("agent start served %+v, %v", trial, err)
			}
		}, reason: "deadline"},
		{name: "agent start after reboot", revert: func(t *testing.T, n *trialNode, _ ManagementTrial) {
			n.clock.reboot()
			if trial, err := SettleManagementTrial(n.root, n.clock); err != nil || trial != nil {
				t.Fatalf("agent start served %+v, %v", trial, err)
			}
		}, reason: "restarted"},
		{name: "confirm after deadline", revert: func(t *testing.T, n *trialNode, _ ManagementTrial) {
			n.clock.advance(time.Minute)
			if _, err := ConfirmManagementTrial(n.root, n.authority, n.clock); !errors.Is(err, ErrManagementTrialReverted) {
				t.Fatalf("late confirm = %v", err)
			}
		}, reason: "deadline"},
		{name: "operator", revert: func(t *testing.T, n *trialNode, _ ManagementTrial) {
			if _, reverted, err := RevertManagementTrial(n.root, "", "reverted from the console", n.clock); err != nil || !reverted {
				t.Fatalf("revert = %v, %v", reverted, err)
			}
		}, reason: "console"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := newTrialNode(t)
			trial := node.mustBegin(t)

			tc.revert(t, node, trial)

			node.assertInstalled(t, managementidentity.TrustedNetwork)
			node.assertActiveRestored(t)
			recorded, found := node.recorded(t)
			if !found || recorded.State != ManagementTrialReverted || !strings.Contains(recorded.RevertReason, tc.reason) {
				t.Fatalf("trial record = %+v; want reverted for %q", recorded, tc.reason)
			}
			if _, err := ConfirmManagementTrial(node.root, node.authority, node.clock); !errors.Is(err, ErrManagementTrialReverted) || !strings.Contains(err.Error(), "rerun") {
				t.Fatalf("confirm after revert = %v", err)
			}
			if served, err := SettleManagementTrial(node.root, node.clock); err != nil || served != nil {
				t.Fatalf("agent start after revert = %+v, %v", served, err)
			}
			node.assertInstalled(t, managementidentity.TrustedNetwork)

			rerun := node.mustBegin(t)
			if rerun.ID == trial.ID {
				t.Fatal("rerun reused the reverted trial")
			}
		})
	}
}

func TestManagementTrialRevertIgnoresEarlierTrial(t *testing.T) {
	node := newTrialNode(t)
	first := node.mustBegin(t)
	if _, _, err := RevertManagementTrial(node.root, first.ID, "operator", node.clock); err != nil {
		t.Fatal(err)
	}
	second := node.mustBegin(t)

	if _, reverted, err := RevertManagementTrial(node.root, first.ID, "late timer", node.clock); err != nil || reverted {
		t.Fatalf("the first trial's timer reverted the second: %v, %v", reverted, err)
	}
	if trial, found := node.recorded(t); !found || trial.ID != second.ID || trial.State != ManagementTrialLive {
		t.Fatalf("trial = %+v; want the second trial still live", trial)
	}
}

func TestManagementTrialExpiredTrialIsReplaced(t *testing.T) {
	node := newTrialNode(t)
	first := node.mustBegin(t)
	node.clock.advance(2 * time.Minute)

	second := node.mustBegin(t)
	if second.ID == first.ID || len(node.armedTrials) != 2 {
		t.Fatalf("begin after an expired trial = %+v, armed %d", second, len(node.armedTrials))
	}
}

// TestManagementTrialInterruptions stops a transition after each durable step,
// as a crash of katlc-agent or the node would, then checks what a restarted
// agent serves and that the client's retry converges.
func TestManagementTrialInterruptions(t *testing.T) {
	for _, tc := range []struct {
		step      string
		confirm   bool
		committed bool
	}{
		{step: "staged"},
		{step: "armed"},
		{step: "active-published", confirm: true},
		{step: "mode-published", confirm: true, committed: true},
	} {
		t.Run(tc.step, func(t *testing.T) {
			for _, restart := range []string{"agent", "reboot"} {
				t.Run(restart, func(t *testing.T) {
					node := newTrialNode(t)
					crash := errors.New("crash")
					managementTrialStep = func(step string) error {
						if step == tc.step {
							return crash
						}
						return nil
					}
					t.Cleanup(func() { managementTrialStep = func(string) error { return nil } })

					var err error
					if tc.confirm {
						node.mustBegin(t)
						_, err = ConfirmManagementTrial(node.root, node.authority, node.clock)
					} else {
						_, err = node.begin()
					}
					if !errors.Is(err, crash) {
						t.Fatalf("transition did not stop at %s: %v", tc.step, err)
					}
					managementTrialStep = func(string) error { return nil }

					if restart == "reboot" {
						node.clock.reboot()
					}
					served, err := SettleManagementTrial(node.root, node.clock)
					if err != nil {
						t.Fatal(err)
					}
					trialLive := tc.confirm && !tc.committed && restart == "agent"
					if (served != nil) != trialLive {
						t.Fatalf("restarted agent serves trial %+v; want trial %v", served, trialLive)
					}
					if tc.committed {
						node.assertInstalled(t, managementidentity.MutualTLS)
						if _, found := node.recorded(t); found {
							t.Fatal("restart left a committed trial's record")
						}
					} else {
						node.assertInstalled(t, managementidentity.TrustedNetwork)
					}
					if restart == "reboot" && !tc.committed {
						node.assertActiveRestored(t)
					}

					// The client retries its confirmation; a node that never
					// recorded a trial or has reverted it asks for a rerun.
					result, err := ConfirmManagementTrial(node.root, node.authority, node.clock)
					if tc.committed || trialLive {
						if err != nil || result.State != ManagementTrialCommitted {
							t.Fatalf("retried confirm = %+v, %v", result, err)
						}
						node.assertInstalled(t, managementidentity.MutualTLS)
						return
					}
					if err == nil || !strings.Contains(err.Error(), "rerun") {
						t.Fatalf("retried confirm = %+v, %v; want a rerun request", result, err)
					}
					node.assertInstalled(t, managementidentity.TrustedNetwork)
					node.mustBegin(t)
				})
			}
		})
	}
}

// TestManagementTrialConfirmRacesRevert runs the confirmation and the deadline
// revert together. Whichever takes the lock first decides, and the other must
// agree with it.
func TestManagementTrialConfirmRacesRevert(t *testing.T) {
	for range 20 {
		node := newTrialNode(t)
		trial := node.mustBegin(t)

		var wg sync.WaitGroup
		var confirmErr, revertErr error
		var reverted bool
		wg.Add(2)
		go func() { defer wg.Done(); _, confirmErr = ConfirmManagementTrial(node.root, node.authority, node.clock) }()
		go func() {
			defer wg.Done()
			_, reverted, revertErr = RevertManagementTrial(node.root, trial.ID, "deadline", node.clock)
		}()
		wg.Wait()
		if revertErr != nil {
			t.Fatal(revertErr)
		}

		switch {
		case confirmErr == nil && !reverted:
			node.assertInstalled(t, managementidentity.MutualTLS)
		case errors.Is(confirmErr, ErrManagementTrialReverted) && reverted:
			node.assertInstalled(t, managementidentity.TrustedNetwork)
			node.assertActiveRestored(t)
		default:
			t.Fatalf("confirm = %v and reverted = %v disagree", confirmErr, reverted)
		}
	}
}

// A rerun after a revert may carry a new server certificate under the same
// authority, so the abandoned version must not pin the old material.
func TestManagementTrialRerunReissuesUnderSameAuthority(t *testing.T) {
	node := newTrialNode(t)
	now := time.Now().UTC()
	bundle, err := managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: "test", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	issue := func() managementidentity.NodeCredentials {
		issued, err := managementidentity.IssueNode(bundle, "cp-1", now, nil)
		if err != nil {
			t.Fatal(err)
		}
		return managementidentity.NodeCredentials{CACertificate: issued.CACertificate, ServerCertificate: issued.ServerCertificate, ServerPrivateKey: issued.ServerPrivateKey}
	}
	node.request.Replacement = issue()
	node.authority, err = managementidentity.CAFingerprint(node.request.Replacement.CACertificate)
	if err != nil {
		t.Fatal(err)
	}
	first := node.mustBegin(t)
	if _, reverted, err := RevertManagementTrial(node.root, first.ID, "deadline", node.clock); err != nil || !reverted {
		t.Fatalf("revert = %v, %v", reverted, err)
	}

	node.request.Replacement = issue()
	node.mustBegin(t)
	if _, err := ConfirmManagementTrial(node.root, node.authority, node.clock); err != nil {
		t.Fatal(err)
	}
	node.assertInstalled(t, managementidentity.MutualTLS)
}
