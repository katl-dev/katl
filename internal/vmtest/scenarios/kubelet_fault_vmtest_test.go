package scenarios

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/operation"
	"github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/katl-dev/katl/internal/vmtest"
	"github.com/katl-dev/katl/internal/vmtest/image/kubelethealth"
	"google.golang.org/protobuf/encoding/protojson"
)

func applyPreCNIKubeletFault(t *testing.T, ctx context.Context, katlctl katlctlRunner, dir, name, diagnostic, badConfig, privateKey, address string) error {
	t.Helper()
	const node = "cp-1"
	data, err := readKubeletNode(ctx, privateKey, address, node, "journalctl -b -u kubelet.service -n 1 --no-pager -o json")
	if err != nil {
		return err
	}
	var cursor struct {
		Value string `json:"__CURSOR"`
	}
	if err := json.Unmarshal(data, &cursor); err != nil {
		return fmt.Errorf("decode kubelet journal cursor: %w", err)
	}
	if cursor.Value == "" {
		return fmt.Errorf("kubelet journal returned no cursor")
	}
	journal, err := os.Create(filepath.Join(dir, name+"-fault-events.jsonl"))
	if err != nil {
		return err
	}
	defer journal.Close()

	checkpoint := name == "checkpoint-policy"
	rolloutPrefix := "vmtest-kubelet-" + name + "-"
	if checkpoint {
		rolloutPrefix = kubelethealth.CheckpointRolloutPrefix
	}
	rollout := fmt.Sprintf("%s%d", rolloutPrefix, time.Now().UnixNano())
	// The cursor lets the observer replay events written before SSH starts.
	// The harness limit also fails a missing trigger before the product deadline.
	applyCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	type observation struct {
		armedID string
		err     error
	}
	observed := make(chan observation, 1)
	go func() {
		id, err := observePreCNIKubeletFault(applyCtx, privateKey, address, cursor.Value, rollout+"-kubelet", diagnostic, checkpoint, journal)
		if err != nil {
			cancel()
		}
		observed <- observation{armedID: id, err: err}
	}()
	start := time.Now()
	stdout, stderr, applyErr := runProofKatlctl(applyCtx, katlctl, dir, name, "cluster", "apply", "--config", badConfig, "--node", node, "--rollout-id", rollout, "-o", "json")
	duration := time.Since(start)
	// Read the independent terminal event before judging the CLI report.
	fault := <-observed
	event, err := readPreCNIKubeletFaultEvent(ctx, node, address, rollout+"-kubelet-"+node)
	if err != nil {
		if fault.err != nil {
			return fmt.Errorf("%s startup observation: %w (terminal operation: %v; apply: %v: %s)", name, fault.err, err, applyErr, stderr)
		}
		return fmt.Errorf("%s terminal operation: %w (apply: %v: %s)", name, err, applyErr, stderr)
	}
	eventJSON, err := (protojson.MarshalOptions{Indent: "  "}).Marshal(event)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+"-terminal-operation.json"), eventJSON, 0o600); err != nil {
		return err
	}
	status := event.GetStatus()
	t.Logf("%s bad apply took %s, operation %s phase %s failure %q", name, duration, event.GetOperationId(), status.GetPhase(), status.GetFailureReason())
	if fault.err != nil {
		return fmt.Errorf("%s startup observation: %w (operation: %s)", name, fault.err, eventJSON)
	}
	if !status.GetTerminal() || status.GetResult() != operation.ResultFailedNeedsRepair || !status.GetRecoveryRequired() {
		return fmt.Errorf("%s did not record a terminal failure requiring repair: %s", name, eventJSON)
	}
	wantPhase := "kubelet-restart-running"
	if checkpoint {
		wantPhase = "post-kubelet-health"
		if fault.armedID != event.GetOperationId() {
			return fmt.Errorf("%s expired %s but observed terminal operation %s", name, fault.armedID, event.GetOperationId())
		}
		if !strings.Contains(status.GetFailureReason(), context.DeadlineExceeded.Error()) {
			return fmt.Errorf("%s did not record deadline expiry: %s", name, eventJSON)
		}
	}
	if status.GetPhase() != wantPhase || status.GetFailureReason() == "" {
		return fmt.Errorf("%s expected a recorded failure in %s: %s", name, wantPhase, eventJSON)
	}

	var failed struct{ Result, NextAction string }
	if err := json.Unmarshal(stdout, &failed); err != nil {
		return fmt.Errorf("%s failure report: %w: %s", name, err, stderr)
	}
	if applyErr == nil || failed.Result != "partial" || strings.TrimSpace(failed.NextAction) == "" {
		return fmt.Errorf("%s did not report incomplete configuration with recovery guidance: %s %s", name, stdout, stderr)
	}
	return nil
}

func observePreCNIKubeletFault(ctx context.Context, privateKey, address, cursor, rollout, diagnostic string, checkpoint bool, journal *os.File) (_ string, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := kubeletNodeCommand(ctx, privateKey, address, "journalctl -b -u kubelet.service -u katlc-agent.service --no-pager -o json --output-fields=MESSAGE,_SYSTEMD_UNIT --follow -n all --after-cursor="+quoteKubeletShellArg(cursor))
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
	var armedID string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := fmt.Fprintln(journal, line); err != nil {
			return "", err
		}
		var entry struct {
			Message json.RawMessage `json:"MESSAGE"`
			Unit    string          `json:"_SYSTEMD_UNIT"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return "", fmt.Errorf("decode kubelet/agent journal entry: %w", err)
		}
		var message string
		if err := json.Unmarshal(entry.Message, &message); err != nil {
			continue // journalctl encodes binary messages as arrays
		}
		if entry.Unit == "kubelet.service" && strings.Contains(message, diagnostic) {
			sawDiagnostic = true
		}
		if entry.Unit == "katlc-agent.service" {
			if arm, ok := strings.CutPrefix(message, kubelethealth.ArmedMessage); ok {
				id, armedRollout, _ := strings.Cut(arm, " ")
				if armedRollout == rollout {
					armedID = id
				}
			}
		}
		if sawDiagnostic && (!checkpoint || armedID != "") {
			if checkpoint {
				command := kubelethealth.AgentPath + " " + kubelethealth.ExpireCommand + " " + quoteKubeletShellArg(armedID)
				if _, err := readKubeletNode(ctx, privateKey, address, "cp-1", command); err != nil {
					return "", fmt.Errorf("expire kubelet health for %s: %w", armedID, err)
				}
			}
			return armedID, nil
		}
	}
	return "", fmt.Errorf("journal follow ended (native diagnostic seen=%t, armed operation=%q, context=%v, read=%v)", sawDiagnostic, armedID, ctx.Err(), scanner.Err())
}

func quoteKubeletShellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func readPreCNIKubeletFaultEvent(ctx context.Context, node, address, requestID string) (*agentapi.OperationEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	connector, err := vmtest.VMTestAgentConnector(vmtest.VMTestManagementClusterName)
	if err != nil {
		return nil, err
	}
	conn, err := connector.Connect(ctx, inventory.PlannedNode{Name: node, Address: address, Access: inventory.Access{Method: "agent"}})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	list, err := conn.Client.ListOperations(ctx, &agentapi.ListOperationsRequest{})
	if err != nil {
		return nil, err
	}
	for _, record := range list.GetOperations() {
		if record.GetClientRequestId() != requestID {
			continue
		}
		// Replay the operation journal so a failure completed before the CLI
		// returned is still observed as a terminal event.
		stream, err := conn.Client.WatchOperation(ctx, &agentapi.WatchOperationRequest{
			OperationId: record.GetOperationId(), ExpectedRequestDigest: record.GetRequestDigest(),
		})
		if err != nil {
			return nil, err
		}
		for {
			event, err := stream.Recv()
			if err != nil {
				return nil, fmt.Errorf("read operation %s events: %w", record.GetOperationId(), err)
			}
			if event.GetTerminal() {
				return event, nil
			}
		}
	}
	return nil, fmt.Errorf("no operation for request %s", requestID)
}
