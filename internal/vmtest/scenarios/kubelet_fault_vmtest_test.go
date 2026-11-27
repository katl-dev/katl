package scenarios

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/vmtest"
	"google.golang.org/protobuf/encoding/protojson"
)

const kubeletFaultNode = "cp-1"

func applyPreCNIKubeletFault(ctx context.Context, katlctl katlctlRunner, dir, name, diagnostic, config, privateKey, address string) (err error) {
	checkpoint := name == "checkpoint-policy"
	if checkpoint {
		const dropIn = "/run/systemd/system/katl-deadline-kubelet-config-health@.timer.d/90-vmtest-disable.conf"
		defer func() {
			_, restoreErr := readKubeletNode(context.WithoutCancel(ctx), privateKey, address, kubeletFaultNode, "rm -f "+dropIn+" && systemctl daemon-reload")
			err = errors.Join(err, restoreErr)
		}()
		if _, err := readKubeletNode(ctx, privateKey, address, kubeletFaultNode,
			"mkdir -p "+filepath.Dir(dropIn)+" && printf %s "+quoteJournalArg(vmtest.DeadlineTimerDisabled)+" >"+dropIn+" && systemctl daemon-reload"); err != nil {
			return err
		}
	}

	cursor, err := kubeletNodeJournalCursor(ctx, privateKey, address, kubeletFaultNode, "kubelet.service")
	if err != nil {
		return err
	}

	// Give the scenario a smaller ceiling than the production timer, so
	// a lost trigger cannot pass by waiting for a normal deadline.
	applyCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	rollout := fmt.Sprintf("vmtest-kubelet-%s-%d", name, time.Now().UnixNano())
	type applyResult struct {
		stdout []byte
		stderr string
		err    error
	}
	done := make(chan applyResult, 1)
	go func() {
		stdout, stderr, err := runProofKatlctl(applyCtx, katlctl, dir, name,
			"cluster", "apply", "--config", config, "--node", kubeletFaultNode, "--rollout-id", rollout, "-o", "json")
		done <- applyResult{stdout, stderr, err}
	}()
	operationID, observeErr := observeKubeletFault(applyCtx, privateKey, address, cursor, rollout+"-kubelet", diagnostic, checkpoint)
	if observeErr != nil {
		cancel()
	}
	apply := <-done
	if observeErr != nil {
		return fmt.Errorf("%s: %w (apply: %v: %s)", name, observeErr, apply.err, apply.stderr)
	}

	status, err := kubeletFaultStatus(ctx, address, rollout+"-kubelet-"+kubeletFaultNode)
	if err != nil {
		return err
	}
	evidence, err := (protojson.MarshalOptions{Indent: "  "}).Marshal(status)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+"-terminal-operation.json"), evidence, 0o600); err != nil {
		return err
	}
	wantPhase := "kubelet-restart-running"
	if checkpoint {
		wantPhase = "post-kubelet-health"
		if operationID != status.GetOperationId() || !strings.Contains(status.GetFailureReason(), context.DeadlineExceeded.Error()) {
			return fmt.Errorf("%s: expiry for operation %s did not produce its deadline failure: %s", name, operationID, evidence)
		}
	}
	if !status.GetTerminal() || status.GetResult() != operation.ResultFailedNeedsRepair || !status.GetRecoveryRequired() ||
		status.GetPhase() != wantPhase || status.GetFailureReason() == "" {
		return fmt.Errorf("%s: expected terminal repair in %s: %s", name, wantPhase, evidence)
	}

	var report struct{ Result, NextAction string }
	if err := json.Unmarshal(apply.stdout, &report); err != nil {
		return fmt.Errorf("%s failure report: %w: %s", name, err, apply.stderr)
	}
	if apply.err == nil || report.Result != "partial" || strings.TrimSpace(report.NextAction) == "" {
		return fmt.Errorf("%s did not report incomplete configuration with recovery guidance: %s %s", name, apply.stdout, apply.stderr)
	}
	return nil
}

func observeKubeletFault(ctx context.Context, privateKey, address, cursor, rollout, diagnostic string, checkpoint bool) (_ string, err error) {
	followCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := kubeletNodeCommand(followCtx, privateKey, address,
		"journalctl -b -u kubelet.service -u katlc-agent.service --no-pager -o json --output-fields=MESSAGE,_SYSTEMD_UNIT --follow -n all --after-cursor="+quoteJournalArg(cursor))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() {
		cancel()
		waitErr := cmd.Wait()
		if err != nil {
			err = fmt.Errorf("%w (ssh: %v): %s", err, waitErr, stderr.String())
		}
	}()

	var sawDiagnostic bool
	var instance, operationID string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		var entry struct {
			Message json.RawMessage `json:"MESSAGE"`
			Unit    string          `json:"_SYSTEMD_UNIT"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return "", err
		}
		var message string
		if err := json.Unmarshal(entry.Message, &message); err != nil {
			continue // journalctl encodes binary messages as arrays
		}
		if entry.Unit == "kubelet.service" && strings.Contains(message, diagnostic) {
			sawDiagnostic = true
		}
		if entry.Unit == "katlc-agent.service" {
			fields := strings.Fields(message)
			if len(fields) == 6 && strings.HasPrefix(message, "armed kubelet-config-health deadline ") && fields[5] == "rollout="+rollout {
				if _, err := hex.DecodeString(fields[3]); len(fields[3]) != 32 || err != nil {
					return "", fmt.Errorf("invalid deadline instance in %q", message)
				}
				instance = fields[3]
				operationID = strings.TrimPrefix(fields[4], "operation=")
			}
		}
		if sawDiagnostic && !checkpoint {
			return "", nil
		}
		if sawDiagnostic && instance != "" {
			timer := "katl-deadline-kubelet-config-health@" + instance + ".timer"
			command := "test \"$(systemctl show --property=ActiveState --value " + timer + ")\" = inactive && systemctl start " + vmtest.DeadlineExpiryUnit(instance)
			if _, err := readKubeletNode(ctx, privateKey, address, kubeletFaultNode, command); err != nil {
				return "", fmt.Errorf("expire kubelet health for %s: %w", operationID, err)
			}
			return operationID, nil
		}
	}
	return "", fmt.Errorf("kubelet journal ended (diagnostic=%t, armed operation=%q, context=%v, read=%v)",
		sawDiagnostic, operationID, ctx.Err(), scanner.Err())
}

func quoteJournalArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func kubeletFaultStatus(ctx context.Context, address, requestID string) (*agentapi.OperationStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	connector, err := vmtest.VMTestAgentConnector(vmtest.VMTestManagementClusterName)
	if err != nil {
		return nil, err
	}
	conn, err := connector.Connect(ctx, agentNode(kubeletFaultNode, address))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	list, err := conn.Client.ListOperations(ctx, &agentapi.ListOperationsRequest{})
	if err != nil {
		return nil, err
	}
	for _, record := range list.GetOperations() {
		if record.GetClientRequestId() == requestID {
			return conn.Client.GetOperation(ctx, &agentapi.GetOperationRequest{OperationId: record.GetOperationId()})
		}
	}
	return nil, fmt.Errorf("no operation for request %s", requestID)
}

func kubeletNodeJournalCursor(ctx context.Context, privateKey, address, node, unit string) (string, error) {
	data, err := readKubeletNode(ctx, privateKey, address, node, "journalctl -b -u "+unit+" -n 1 --no-pager -o json")
	if err != nil {
		return "", err
	}
	var cursor struct {
		Value string `json:"__CURSOR"`
	}
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Value == "" {
		return "", fmt.Errorf("read %s %s journal cursor: %v: %s", node, unit, err, data)
	}
	return cursor.Value, nil
}

// assertKubeletHealthTimerArmed checks, on a path without the test drop-in,
// that post-apply kubelet health started its production timer through
// systemd, that the timer carries the production value, and that a healthy
// apply disarmed it without expiry.
func assertKubeletHealthTimerArmed(ctx context.Context, privateKey, address, node, agentCursor string) error {
	armed, err := readKubeletNode(ctx, privateKey, address, node,
		"journalctl -b -u katlc-agent.service --no-pager -o cat --after-cursor="+quoteJournalArg(agentCursor)+" --grep='^armed kubelet-config-health deadline '")
	if err != nil {
		return fmt.Errorf("%s kubelet apply did not arm its health deadline: %w", node, err)
	}
	lines := strings.Split(strings.TrimSpace(string(armed)), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return fmt.Errorf("%s armed deadline record is malformed: %q", node, armed)
	}
	instance := fields[3]
	if _, err := hex.DecodeString(instance); len(instance) != 32 || err != nil {
		return fmt.Errorf("%s armed deadline has invalid instance: %q", node, armed)
	}
	timer := "katl-deadline-kubelet-config-health@" + instance + ".timer"

	// PID 1's own record shows the timer started, independent of the agent's log.
	started, err := readKubeletNode(ctx, privateKey, address, node, "journalctl -b -u "+timer+" --no-pager -q -o cat")
	if err != nil {
		return err
	}
	if !strings.Contains(string(started), "Started") {
		return fmt.Errorf("systemd did not start %s on %s:\n%s", timer, node, started)
	}
	config, err := readKubeletNode(ctx, privateKey, address, node, "systemctl show --property=TimersMonotonic --property=DropInPaths "+timer)
	if err != nil {
		return err
	}
	if !strings.Contains(string(config), "OnActiveUSec=2min") || !strings.Contains(string(config), "DropInPaths=\n") {
		return fmt.Errorf("%s on %s does not carry only the production deadline:\n%s", timer, node, config)
	}
	expired, err := readKubeletNode(ctx, privateKey, address, node, "journalctl -b -u "+vmtest.DeadlineExpiryUnit(instance)+" --no-pager -q -o cat")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(expired)) != "" {
		return fmt.Errorf("healthy kubelet apply on %s expired its deadline:\n%s", node, expired)
	}
	return nil
}
