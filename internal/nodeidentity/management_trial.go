package nodeidentity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/managementidentity"
	"github.com/katl-dev/katl/internal/persistedrecord"
)

// A management trial moves a node to a replacement authority only after a
// client proves it holds the matching credentials. Until it is confirmed, the
// installed mode, active link and mode file stay authoritative; the trial
// record only lets katlc-agent serve the replacement until its deadline in the
// boot that began it.
//
// The installed state is committed once the mode file names mTLS and active
// names the trial's version. Confirm publishes the mode file last, so every
// interruption before it leaves the previous mode in force and every
// interruption after it leaves the replacement in force.

const (
	managementTrialFile          = "transition.json"
	managementTrialRecordType    = "katl.management.transition"
	managementTrialRecordVersion = 1

	defaultManagementTrialWindow = 5 * time.Minute
)

// A trial window outside these bounds is refused. The workstation checks them
// too, before it changes anything.
const (
	MinimumManagementTrialWindow = 30 * time.Second
	MaximumManagementTrialWindow = 30 * time.Minute
)

type ManagementTrialState string

const (
	ManagementTrialLive      ManagementTrialState = "trial"
	ManagementTrialReverted  ManagementTrialState = "reverted"
	ManagementTrialCommitted ManagementTrialState = "committed"
)

// ErrManagementTrialReverted reports that the trial a client is confirming no
// longer serves the replacement authority.
var ErrManagementTrialReverted = errors.New("management trial expired and reverted")

// ManagementTransition asks a node to serve a replacement authority on trial.
type ManagementTransition struct {
	NodeName               string
	ExpectedEnrollmentID   string
	ExpectedMachineID      string
	ExpectedAuthentication managementidentity.Authentication
	Replacement            managementidentity.NodeCredentials
	Window                 time.Duration
}

// ManagementTrial is the persisted trial record. Committed trials are not
// stored; that state only describes a result.
type ManagementTrial struct {
	ID              string                            `json:"id"`
	State           ManagementTrialState              `json:"state"`
	TargetAuthority string                            `json:"targetAuthority"`
	From            managementidentity.Authentication `json:"from"`
	PreviousActive  string                            `json:"previousActive,omitempty"`
	BootID          string                            `json:"bootID"`
	// DeadlineSinceBoot is measured on CLOCK_BOOTTIME so wall-clock changes
	// cannot extend or cut short a trial.
	DeadlineSinceBoot time.Duration `json:"deadlineSinceBoot"`
	Deadline          time.Time     `json:"deadline"`
	StartedAt         time.Time     `json:"startedAt"`
	RevertedAt        *time.Time    `json:"revertedAt,omitempty"`
	RevertReason      string        `json:"revertReason,omitempty"`
}

// TrialClock observes the boot and its elapsed time for trial deadlines.
type TrialClock interface {
	BootID() (string, error)
	SinceBoot() (time.Duration, error)
	Now() time.Time
}

type SystemTrialClock struct{}

func (SystemTrialClock) BootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read boot id: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func (SystemTrialClock) Now() time.Time { return time.Now().UTC() }

// managementTrialStep marks each durable step so tests can stop a transition
// where a crash could.
var managementTrialStep = func(string) error { return nil }

// BeginManagementTrial stages the replacement and records a live trial. arm
// must schedule the revert for the returned deadline; it runs before the
// record is written so a persisted live trial always has a revert scheduled.
// Repeating the request while its trial is live returns the same trial without
// arming again.
func BeginManagementTrial(root string, request ManagementTransition, clock TrialClock, arm func(ManagementTrial) error) (ManagementTrial, error) {
	if strings.TrimSpace(root) == "" {
		return ManagementTrial{}, fmt.Errorf("runtime root is required")
	}
	window := request.Window
	if window == 0 {
		window = defaultManagementTrialWindow
	}
	if window < MinimumManagementTrialWindow || window > MaximumManagementTrialWindow {
		return ManagementTrial{}, fmt.Errorf("management trial window must be between %s and %s", MinimumManagementTrialWindow, MaximumManagementTrialWindow)
	}
	dir := filepath.Join(filepath.Clean(root), managementDirectory)
	unlock, err := lockManagementIdentity(dir)
	if err != nil {
		return ManagementTrial{}, err
	}
	defer unlock()

	enrollment, err := ReadEnrollment(root)
	if err != nil {
		return ManagementTrial{}, err
	}
	switch {
	case enrollment.InventoryNodeName != strings.TrimSpace(request.NodeName):
		return ManagementTrial{}, fmt.Errorf("node is enrolled as %q, not %q", enrollment.InventoryNodeName, request.NodeName)
	case enrollment.ID != strings.TrimSpace(request.ExpectedEnrollmentID):
		return ManagementTrial{}, fmt.Errorf("node enrollment changed; inspect the node before retrying")
	case enrollment.MachineID != strings.TrimSpace(request.ExpectedMachineID):
		return ManagementTrial{}, fmt.Errorf("node machine ID changed; expected %q", request.ExpectedMachineID)
	}
	now := clock.Now().UTC()
	if err := managementidentity.ValidateNode(request.Replacement, enrollment.InventoryNodeName, now); err != nil {
		return ManagementTrial{}, fmt.Errorf("validate replacement management identity: %w", err)
	}
	target, err := managementidentity.CAFingerprint(request.Replacement.CACertificate)
	if err != nil {
		return ManagementTrial{}, err
	}
	committed, err := managementCommitted(root, target)
	if err != nil {
		return ManagementTrial{}, err
	}
	if committed {
		return ManagementTrial{State: ManagementTrialCommitted, TargetAuthority: target}, nil
	}
	installed, err := ManagementAuthentication(root)
	if err != nil {
		return ManagementTrial{}, err
	}
	if installed != request.ExpectedAuthentication {
		return ManagementTrial{}, fmt.Errorf("node uses %s management, not %s", installed, request.ExpectedAuthentication)
	}
	if installed != managementidentity.TrustedNetwork {
		return ManagementTrial{}, fmt.Errorf("only a trusted-network node can begin a management transition; rotate mTLS credentials with katlctl management identity rotate")
	}

	bootID, err := clock.BootID()
	if err != nil {
		return ManagementTrial{}, err
	}
	sinceBoot, err := clock.SinceBoot()
	if err != nil {
		return ManagementTrial{}, err
	}
	existing, found, err := readManagementTrial(dir)
	if err != nil {
		return ManagementTrial{}, fmt.Errorf("%w; clear it with katlc agent management-trial revert on the node", err)
	}
	if found && existing.State == ManagementTrialLive {
		if existing.BootID == bootID && sinceBoot < existing.DeadlineSinceBoot {
			if existing.TargetAuthority != target {
				return ManagementTrial{}, fmt.Errorf("a management transition to authority %s is in progress until %s; confirm it, or revert it with katlc agent management-trial revert", existing.TargetAuthority, existing.Deadline.Format(time.RFC3339))
			}
			if err := verifyManagementVersion(filepath.Join(dir, "versions", target), request.Replacement); err != nil {
				return ManagementTrial{}, err
			}
			return existing, nil
		}
		if _, err := revertManagementTrialLocked(dir, existing, "a new transition replaced an expired trial", now); err != nil {
			return ManagementTrial{}, err
		}
	}

	previousActive, err := os.Readlink(filepath.Join(dir, "active"))
	if err != nil && !os.IsNotExist(err) {
		return ManagementTrial{}, fmt.Errorf("read active management identity: %w", err)
	}
	versions := filepath.Join(dir, "versions")
	if err := os.MkdirAll(versions, 0o700); err != nil {
		return ManagementTrial{}, fmt.Errorf("create management identity versions: %w", err)
	}
	// No live trial names the target version here, so unless active does, it
	// was left by a begin that failed after staging. Discard it so material
	// reissued under the same authority can be staged.
	if filepath.Join("versions", target) != previousActive {
		if err := removeManagementVersion(dir, target); err != nil {
			return ManagementTrial{}, err
		}
	}
	if err := stageManagementVersion(versions, target, request.Replacement); err != nil {
		return ManagementTrial{}, err
	}
	if err := managementTrialStep("staged"); err != nil {
		return ManagementTrial{}, err
	}
	id, err := generateIdentity(nil)
	if err != nil {
		return ManagementTrial{}, fmt.Errorf("generate management trial id: %w", err)
	}
	trial := ManagementTrial{
		ID: id, State: ManagementTrialLive, TargetAuthority: target, From: installed, PreviousActive: previousActive,
		BootID: bootID, DeadlineSinceBoot: sinceBoot + window, Deadline: now.Add(window), StartedAt: now,
	}
	if err := arm(trial); err != nil {
		return ManagementTrial{}, fmt.Errorf("schedule management trial revert: %w", err)
	}
	if err := managementTrialStep("armed"); err != nil {
		return ManagementTrial{}, err
	}
	if err := writeManagementTrial(dir, trial); err != nil {
		return ManagementTrial{}, err
	}
	return trial, nil
}

// ConfirmManagementTrial commits the live trial for authority. A non-empty ID
// binds the confirmation to that trial under the identity lock. Confirming
// an already committed authority succeeds again. The result names the trial
// it committed, if any, so its scheduled revert can be cancelled.
func ConfirmManagementTrial(root, authority, id string, clock TrialClock) (ManagementTrial, error) {
	dir := filepath.Join(filepath.Clean(root), managementDirectory)
	unlock, err := lockManagementIdentity(dir)
	if err != nil {
		return ManagementTrial{}, err
	}
	defer unlock()

	trial, found, err := readManagementTrial(dir)
	if err != nil {
		return ManagementTrial{}, err
	}
	if id != "" && found && trial.ID != id {
		return ManagementTrial{}, fmt.Errorf("the management trial changed; rerun the transition")
	}
	committed, err := managementCommitted(root, authority)
	if err != nil {
		return ManagementTrial{}, err
	}
	if committed {
		result := ManagementTrial{State: ManagementTrialCommitted, TargetAuthority: authority}
		if found && trial.State == ManagementTrialLive && trial.TargetAuthority == authority {
			result.ID = trial.ID
			if err := removeManagementTrial(dir); err != nil {
				return ManagementTrial{}, err
			}
		}
		return result, nil
	}
	switch {
	case !found:
		return ManagementTrial{}, fmt.Errorf("no management transition to authority %s is in progress; rerun the transition", authority)
	case trial.TargetAuthority != authority:
		return ManagementTrial{}, fmt.Errorf("the management transition on this node targets authority %s, not %s", trial.TargetAuthority, authority)
	case trial.State == ManagementTrialReverted:
		return trial, fmt.Errorf("%w (%s); rerun the transition", ErrManagementTrialReverted, trial.RevertReason)
	}
	if reason, err := managementTrialExpiry(trial, clock); err != nil {
		return ManagementTrial{}, err
	} else if reason != "" {
		reverted, err := revertManagementTrialLocked(dir, trial, reason, clock.Now())
		if err != nil {
			return ManagementTrial{}, err
		}
		return reverted, fmt.Errorf("%w (%s); rerun the transition", ErrManagementTrialReverted, reason)
	}

	if err := publishActiveManagementVersion(dir, filepath.Join("versions", authority)); err != nil {
		return ManagementTrial{}, err
	}
	if err := managementTrialStep("active-published"); err != nil {
		return ManagementTrial{}, err
	}
	if err := persistedrecord.WriteFileAtomic(filepath.Join(filepath.Clean(root), ManagementAuthenticationPath), []byte(string(managementidentity.MutualTLS)+"\n"), 0o600); err != nil {
		return ManagementTrial{}, fmt.Errorf("publish management authentication: %w", err)
	}
	if err := managementTrialStep("mode-published"); err != nil {
		return ManagementTrial{}, err
	}
	if err := removeManagementTrial(dir); err != nil {
		return ManagementTrial{}, err
	}
	return ManagementTrial{ID: trial.ID, State: ManagementTrialCommitted, TargetAuthority: authority}, nil
}

// RevertManagementTrial is the single owner of reverting a live trial. It
// reports whether it reverted, so a caller knows katlc-agent must re-serve the
// installed mode. A non-empty id limits a readable record to that trial, so a
// revert scheduled for an earlier trial cannot cut a later one short.
//
// An unreadable record is moved aside instead, whatever id names, so any
// revert ends a later trial whose record cannot be read: the mode
// file and active link alone decide the installed mode, and katlc-agent already
// serves that mode while the record cannot be read.
func RevertManagementTrial(root, id, reason string, clock TrialClock) (ManagementTrial, bool, error) {
	dir := filepath.Join(filepath.Clean(root), managementDirectory)
	unlock, err := lockManagementIdentity(dir)
	if err != nil {
		return ManagementTrial{}, false, err
	}
	defer unlock()

	trial, found, err := readManagementTrial(dir)
	if err != nil {
		moved, moveErr := quarantineManagementTrial(dir, clock.Now())
		if moveErr != nil {
			return ManagementTrial{}, false, errors.Join(err, moveErr)
		}
		return ManagementTrial{State: ManagementTrialReverted, RevertReason: fmt.Sprintf("moved the unreadable management transition record to %s (%v)", moved, err)}, true, nil
	}
	if !found || trial.State != ManagementTrialLive || (id != "" && trial.ID != id) {
		return trial, false, nil
	}
	committed, err := managementCommitted(root, trial.TargetAuthority)
	if err != nil {
		return ManagementTrial{}, false, err
	}
	if committed {
		return ManagementTrial{State: ManagementTrialCommitted, TargetAuthority: trial.TargetAuthority}, false, removeManagementTrial(dir)
	}
	reverted, err := revertManagementTrialLocked(dir, trial, reason, clock.Now())
	return reverted, err == nil, err
}

// SettleManagementTrial runs before katlc-agent loads its credentials. It
// finishes a committed trial, reverts one from an earlier boot or past its
// deadline, and returns the trial the agent should serve, if any.
func SettleManagementTrial(root string, clock TrialClock) (*ManagementTrial, error) {
	dir := filepath.Join(filepath.Clean(root), managementDirectory)
	unlock, err := lockManagementIdentity(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	trial, found, err := readManagementTrial(dir)
	if err != nil || !found || trial.State != ManagementTrialLive {
		return nil, err
	}
	committed, err := managementCommitted(root, trial.TargetAuthority)
	if err != nil {
		return nil, err
	}
	if committed {
		return nil, removeManagementTrial(dir)
	}
	reason, err := managementTrialExpiry(trial, clock)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		_, err := revertManagementTrialLocked(dir, trial, reason, clock.Now())
		return nil, err
	}
	return &trial, nil
}

// ReadManagementTrial returns the recorded trial without changing it.
func ReadManagementTrial(root string) (ManagementTrial, bool, error) {
	return readManagementTrial(filepath.Join(filepath.Clean(root), managementDirectory))
}

// InstalledManagementAuthority returns the fingerprint of the authority an mTLS
// node trusts outside any trial.
func InstalledManagementAuthority(root string) (string, error) {
	caPath, _, _, err := ManagementCredentialsPaths(root)
	if err != nil {
		return "", err
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return "", fmt.Errorf("read installed management authority: %w", err)
	}
	return managementidentity.CAFingerprint(string(ca))
}

func managementTrialExpiry(trial ManagementTrial, clock TrialClock) (string, error) {
	bootID, err := clock.BootID()
	if err != nil {
		return "", err
	}
	if bootID != trial.BootID {
		return "the node restarted before the transition was confirmed", nil
	}
	sinceBoot, err := clock.SinceBoot()
	if err != nil {
		return "", err
	}
	if sinceBoot >= trial.DeadlineSinceBoot {
		return "the transition was not confirmed before its deadline", nil
	}
	return "", nil
}

func managementCommitted(root, authority string) (bool, error) {
	mode, err := ManagementAuthentication(root)
	if err != nil || mode != managementidentity.MutualTLS {
		return false, err
	}
	target, err := os.Readlink(filepath.Join(filepath.Clean(root), managementDirectory, "active"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read active management identity: %w", err)
	}
	return target == filepath.Join("versions", authority), nil
}

func revertManagementTrialLocked(dir string, trial ManagementTrial, reason string, now time.Time) (ManagementTrial, error) {
	active, err := os.Readlink(filepath.Join(dir, "active"))
	if err != nil && !os.IsNotExist(err) {
		return ManagementTrial{}, fmt.Errorf("read active management identity: %w", err)
	}
	if active != trial.PreviousActive {
		if err := publishActiveManagementVersion(dir, trial.PreviousActive); err != nil {
			return ManagementTrial{}, err
		}
	}
	// Discard the abandoned version so a rerun can stage fresh or repaired
	// material under the same authority.
	if filepath.Join("versions", trial.TargetAuthority) != trial.PreviousActive {
		if err := removeManagementVersion(dir, trial.TargetAuthority); err != nil {
			return ManagementTrial{}, err
		}
	}
	revertedAt := now.UTC()
	trial.State = ManagementTrialReverted
	trial.RevertedAt = &revertedAt
	trial.RevertReason = reason
	if err := writeManagementTrial(dir, trial); err != nil {
		return ManagementTrial{}, err
	}
	return trial, nil
}

func readManagementTrial(dir string) (ManagementTrial, bool, error) {
	path := filepath.Join(dir, managementTrialFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return ManagementTrial{}, false, nil
	}
	envelope, err := persistedrecord.ReadEnvelope(path)
	if err != nil {
		return ManagementTrial{}, false, fmt.Errorf("read management trial: %w", err)
	}
	if envelope.RecordType != managementTrialRecordType || envelope.RecordVersion != managementTrialRecordVersion {
		return ManagementTrial{}, false, fmt.Errorf("%w: management trial %s/v%d", persistedrecord.ErrUnsupportedRecord, envelope.RecordType, envelope.RecordVersion)
	}
	trial, err := persistedrecord.DecodePayload[ManagementTrial](envelope)
	if err != nil {
		return ManagementTrial{}, false, fmt.Errorf("read management trial: %w", err)
	}
	return trial, true, nil
}

func writeManagementTrial(dir string, trial ManagementTrial) error {
	payload, err := persistedrecord.MarshalCanonical(trial)
	if err != nil {
		return err
	}
	envelope := persistedrecord.Envelope{RecordType: managementTrialRecordType, RecordVersion: managementTrialRecordVersion, Payload: payload}
	if err := persistedrecord.WriteEnvelopeAtomic(filepath.Join(dir, managementTrialFile), envelope, 0o600); err != nil {
		return fmt.Errorf("write management trial: %w", err)
	}
	return nil
}

// removeManagementVersion durably removes versions/authority, if present.
func removeManagementVersion(dir, authority string) error {
	if err := os.RemoveAll(filepath.Join(dir, "versions", authority)); err != nil {
		return fmt.Errorf("remove abandoned management identity version: %w", err)
	}
	if err := syncManagementDirectory(filepath.Join(dir, "versions")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// quarantineManagementTrial keeps an unreadable record for diagnosis under a
// name no reader opens.
func quarantineManagementTrial(dir string, now time.Time) (string, error) {
	moved := filepath.Join(dir, fmt.Sprintf("%s.unreadable-%d", managementTrialFile, now.UTC().UnixNano()))
	if err := os.Rename(filepath.Join(dir, managementTrialFile), moved); err != nil {
		return "", fmt.Errorf("move unreadable management trial aside: %w", err)
	}
	return moved, syncManagementDirectory(dir)
}

func removeManagementTrial(dir string) error {
	if err := os.Remove(filepath.Join(dir, managementTrialFile)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove management trial: %w", err)
	}
	return syncManagementDirectory(dir)
}
