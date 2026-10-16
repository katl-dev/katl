package kubeletstartup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/generation"
)

const recordPath = "/run/katl/kubelet-startup.json"

// Record binds second-precision Node reports to a kubelet service invocation.
// It is ephemeral: every invocation replaces its predecessor's boundary.
type Record struct {
	InvocationID  string    `json:"invocationID"`
	BootID        string    `json:"bootID"`
	ConfextTarget string    `json:"confextTarget"`
	NotBefore     time.Time `json:"notBefore"`
}

// Prepare runs in ExecStartPre, after the previous invocation has stopped.
// Ordinary realtime ordering across invocations is required; native Node
// timestamps cannot establish process freshness across arbitrary clock steps.
func Prepare(ctx context.Context, root, invocationID string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if err := validateInvocation(invocationID); err != nil {
		return Record{}, err
	}
	boot, err := readBootID(root)
	if err != nil {
		return Record{}, err
	}
	confext, err := confextTarget(root)
	if err != nil {
		return Record{}, err
	}
	start := time.Now()
	boundary := start.Truncate(time.Second).Add(time.Second)
	// Separate the old process's reports from the new process's first report
	// without depending on the operator's nodeStatusReportFrequency.
	timer := time.NewTimer(time.Until(boundary))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return Record{}, ctx.Err()
	case <-timer.C:
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	now := time.Now()
	wallElapsed := time.Duration(now.UnixNano() - start.UnixNano())
	step := wallElapsed - now.Sub(start)
	if now.Before(boundary) || step > 100*time.Millisecond || step < -100*time.Millisecond {
		return Record{}, fmt.Errorf("clock changed while preparing kubelet startup; synchronize the node clock and restart kubelet")
	}
	record := Record{InvocationID: invocationID, BootID: boot, ConfextTarget: confext, NotBefore: boundary.UTC()}
	data, err := json.Marshal(record)
	if err != nil {
		return Record{}, err
	}
	path := rooted(root, recordPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Record{}, err
	}
	// A reader must see either the old complete invocation or the new complete
	// invocation. A mismatch with systemd never authorizes startup completion.
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return Record{}, err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return Record{}, err
	}
	return record, nil
}

func Read(root string) (Record, error) {
	data, err := os.ReadFile(rooted(root, recordPath))
	if err != nil {
		return Record{}, fmt.Errorf("read kubelet startup evidence: %w", err)
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("decode kubelet startup evidence: %w", err)
	}
	if err := record.validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Verify checks a native report against the service invocation being observed.
// Callers must also verify that this invocation remains active after probing.
func (r Record) Verify(root, invocationID, nodeBootID string, heartbeat, now time.Time) error {
	if err := r.validate(); err != nil {
		return err
	}
	boot, err := readBootID(root)
	if err != nil {
		return err
	}
	if r.InvocationID != invocationID || r.BootID != boot || nodeBootID != boot {
		return fmt.Errorf("kubelet startup evidence belongs to another invocation or boot")
	}
	if heartbeat.IsZero() || heartbeat.Before(r.NotBefore) {
		return fmt.Errorf("waiting for a Node health report from the current kubelet invocation")
	}
	if now.Before(r.NotBefore) || heartbeat.After(now) {
		return fmt.Errorf("Node health report is future-dated; synchronize the node clock and restart kubelet")
	}
	return nil
}

// ConfextChanged reports whether kubelet started before the mounted node
// configuration changed, requiring its static-pod watcher to be rebound.
func (r Record) ConfextChanged(root string) (bool, error) {
	target, err := confextTarget(root)
	return target != r.ConfextTarget, err
}

func validateInvocation(id string) error {
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return fmt.Errorf("kubelet startup requires a systemd invocation ID")
	}
	return nil
}

func (r Record) validate() error {
	if err := validateInvocation(r.InvocationID); err != nil {
		return err
	}
	if r.BootID == "" || r.ConfextTarget == "" || r.NotBefore.IsZero() || r.NotBefore.Nanosecond() != 0 {
		return fmt.Errorf("incomplete kubelet startup evidence")
	}
	return nil
}

func readBootID(root string) (string, error) {
	data, err := os.ReadFile(rooted(root, "/proc/sys/kernel/random/boot_id"))
	if err != nil {
		return "", err
	}
	boot := strings.TrimSpace(string(data))
	if boot == "" {
		return "", fmt.Errorf("kernel boot identity is empty")
	}
	return boot, nil
}

func confextTarget(root string) (string, error) {
	return os.Readlink(rooted(root, filepath.Join(generation.DefaultConfextsActivationDir, generation.GeneratedConfextName)))
}

func rooted(root, path string) string {
	return filepath.Join(root, strings.TrimPrefix(path, "/"))
}
