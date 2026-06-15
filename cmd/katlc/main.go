package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/installer/operation"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

type toolResult struct {
	Stdout     []byte
	Stderr     []byte
	ExitStatus int
	Err        error
}

var (
	now             = func() time.Time { return time.Now().UTC() }
	systemdInvokeID = func() string { return strings.TrimSpace(os.Getenv("INVOCATION_ID")) }
	currentBootID   = readCurrentBootID
	liveInvocation  = systemdLiveInvocation
	runToolCommand  = defaultRunToolCommand
)

const (
	defaultOperationToolTimeout = 25 * time.Minute
	maxOperationToolTimeout     = 25 * time.Minute
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "katlc: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("command is required")
	}
	if args[0] == "--version" || args[0] == "version" {
		fmt.Fprintf(stdout, "katlc version=%s commit=%s date=%s\n", version, commit, date)
		return nil
	}
	if args[0] != "operation" {
		return fmt.Errorf("unsupported command %q", strings.Join(args, " "))
	}
	return runOperation(ctx, args[1:], stdout, stderr)
}

func runOperation(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("operation command is required")
	}
	switch args[0] {
	case "reconcile":
		return runOperationReconcile(args[1:], stdout, stderr)
	case "execute":
		return runOperationExecute(ctx, args[1:], stdout, stderr)
	case "run-tool":
		return runOperationTool(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unsupported operation command %q", args[0])
	}
}

func runOperationReconcile(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katlc operation reconcile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "/", "runtime root containing /var/lib/katl")
	boot := flags.Bool("boot", false, "reconcile operation records at boot")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if !*boot {
		return fmt.Errorf("--boot is required; non-boot reconciliation is not implemented")
	}
	store, err := operation.NewStore(operationStoreRoot(*root))
	if err != nil {
		return err
	}
	report, err := store.ReconcileBoot(now(), currentBootID(), liveInvocation)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal operation reconcile report: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func runOperationExecute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katlc operation execute", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "/", "runtime root containing /var/lib/katl")
	operationID := flags.String("operation-id", "", "operation id to execute under systemd")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if strings.TrimSpace(*operationID) == "" {
		return fmt.Errorf("--operation-id is required")
	}
	store, err := operation.NewStore(operationStoreRoot(*root))
	if err != nil {
		return err
	}
	record, err := store.Read(*operationID)
	if err != nil {
		return err
	}
	plan, err := readToolPlan(operationStoreRoot(*root), record.OperationID)
	if err != nil {
		markErr := markExecuteRefused(store, record.OperationID, err)
		if markErr != nil {
			return errors.Join(err, markErr)
		}
		return err
	}
	toolArgs := []string{
		"--root", *root,
		"--operation-id", record.OperationID,
		"--phase", plan.Phase,
	}
	if strings.TrimSpace(plan.MarkerID) != "" {
		toolArgs = append(toolArgs, "--marker-id", plan.MarkerID)
	}
	if strings.TrimSpace(plan.Timeout) != "" {
		toolArgs = append(toolArgs, "--timeout", plan.Timeout)
	}
	for _, scope := range plan.MutationScopes {
		toolArgs = append(toolArgs, "--mutation-scope", scope)
	}
	toolArgs = append(toolArgs, "--")
	toolArgs = append(toolArgs, plan.Argv...)
	return runOperationTool(ctx, toolArgs, stdout, stderr)
}

func runOperationTool(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("katlc operation run-tool", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "/", "runtime root containing /var/lib/katl")
	operationID := flags.String("operation-id", "", "operation id")
	phase := flags.String("phase", "", "operation phase")
	markerID := flags.String("marker-id", "", "optional pre-exec mutation marker id")
	timeout := flags.Duration("timeout", defaultOperationToolTimeout, "maximum tool runtime")
	var scopes stringList
	flags.Var(&scopes, "mutation-scope", "expected mutation scope; may be repeated")
	if err := flags.Parse(args); err != nil {
		return err
	}
	argv := flags.Args()
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if strings.TrimSpace(*operationID) == "" {
		return fmt.Errorf("--operation-id is required")
	}
	if strings.TrimSpace(*phase) == "" {
		return fmt.Errorf("--phase is required")
	}
	if len(argv) == 0 {
		return fmt.Errorf("tool argv is required after --")
	}
	if strings.TrimSpace(*markerID) == "" {
		*markerID = generatedMarkerID(*phase, argv)
	}
	if *timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	if *timeout > maxOperationToolTimeout {
		return fmt.Errorf("--timeout must not exceed %s", maxOperationToolTimeout)
	}

	store, err := operation.NewStore(operationStoreRoot(*root))
	if err != nil {
		return err
	}
	systemdID := systemdInvokeID()
	invocationID := firstNonEmpty(systemdID, *markerID)
	bootID := ""
	unitName := ""
	if systemdID != "" {
		bootID = currentBootID()
		unitName = firstNonEmpty(os.Getenv("KATL_OPERATION_UNIT"), "katl-operation@"+*operationID+".service")
	}
	startedAt := now()
	argvDigest := digestArgv(argv)
	marker := operation.PreExecMutationMarker{
		MarkerID:               *markerID,
		InvocationID:           invocationID,
		Phase:                  *phase,
		Tool:                   filepath.Base(argv[0]),
		ArgvDigest:             argvDigest,
		ExpectedMutationScopes: scopes.Values(),
		MarkedAt:               startedAt,
	}
	if _, err := store.Update(*operationID, *markerID+"-start", "pre-exec-mutation", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.ExternalMutationStarted = true
		record.PreExecMutationMarkers = append(record.PreExecMutationMarkers, marker)
		record.MutationScopes = appendMissing(record.MutationScopes, scopes.Values()...)
		record.Invocations = append(record.Invocations, operation.InvocationRecord{
			InvocationID:        *markerID,
			SystemdInvocationID: systemdID,
			UnitName:            unitName,
			BootID:              bootID,
			StartedAt:           startedAt,
			Result:              "started",
		})
		record.Phase = *phase
		record.UpdatedAt = startedAt
		return record, nil
	}); err != nil {
		return err
	}

	toolCtx := ctx
	cancel := func() {}
	if *timeout > 0 {
		toolCtx, cancel = context.WithTimeout(ctx, *timeout)
	}
	defer cancel()
	result := runToolCommand(toolCtx, argv)
	if errors.Is(toolCtx.Err(), context.DeadlineExceeded) && result.Err == nil {
		result.Err = toolCtx.Err()
		result.ExitStatus = -1
	}
	completedAt := now()
	timedOut := errors.Is(toolCtx.Err(), context.DeadlineExceeded) || errors.Is(result.Err, context.DeadlineExceeded)
	resultText := exitResult(result)
	if timedOut {
		resultText = operation.ResultTimedOut
	}
	if len(result.Stdout) > 0 {
		if _, err := store.AddDiagnosticArtifact(*operationID, *markerID+"-stdout", []byte(inventory.Redact(string(result.Stdout))), completedAt); err != nil {
			return err
		}
	}
	if len(result.Stderr) > 0 {
		if _, err := store.AddDiagnosticArtifact(*operationID, *markerID+"-stderr", []byte(inventory.Redact(string(result.Stderr))), completedAt); err != nil {
			return err
		}
	}
	updated, updateErr := store.Update(*operationID, *markerID+"-complete", "tool-complete", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		completeInvocation(record.Invocations, *markerID, completedAt, resultText)
		record.MutatingToolRan = true
		record.MutatingToolInvocations = appendMissing(record.MutatingToolInvocations, inventory.Redact(strings.Join(argv, " ")))
		record.Phase = *phase
		if timedOut {
			record.Terminal = true
			record.CompletedAt = &completedAt
			record.RecoveryRequired = true
			record.Result = operation.ResultFailedNeedsRepair
			record.Interruption = operation.ResultTimedOut
			record.NextAction = "explicit repair required after operation timeout"
			record.FailureReason = inventory.Redact(toolFailure(result))
		} else if result.Err == nil && result.ExitStatus == 0 {
			record.CompletedPhases = appendMissing(record.CompletedPhases, *phase)
			if len(record.CompletedPhases) > record.PhaseIndex {
				record.PhaseIndex = len(record.CompletedPhases)
			}
		} else {
			record.RecoveryRequired = true
			record.Result = operation.ResultFailedNeedsRepair
			record.FailureReason = inventory.Redact(toolFailure(result))
		}
		record.UpdatedAt = completedAt
		return record, nil
	})
	if updateErr != nil {
		return updateErr
	}
	fmt.Fprintf(stdout, "katlc operation run-tool operationID=%s phase=%s result=%s\n", updated.OperationID, *phase, resultText)
	if result.Err != nil {
		return fmt.Errorf("run %s: %s", argv[0], inventory.Redact(result.Err.Error()))
	}
	if result.ExitStatus != 0 {
		return fmt.Errorf("run %s: exit status %d", argv[0], result.ExitStatus)
	}
	return nil
}

func defaultRunToolCommand(ctx context.Context, argv []string) toolResult {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	status := 0
	if err != nil {
		status = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			status = exitErr.ExitCode()
		}
	}
	return toolResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitStatus: status, Err: err}
}

type toolPlan struct {
	Phase          string   `json:"phase"`
	MarkerID       string   `json:"markerID,omitempty"`
	Timeout        string   `json:"timeout,omitempty"`
	MutationScopes []string `json:"mutationScopes,omitempty"`
	Argv           []string `json:"argv"`
}

func readToolPlan(storeRoot string, operationID string) (toolPlan, error) {
	path := filepath.Join(storeRoot, operationID, "run-tool.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return toolPlan{}, fmt.Errorf("read operation tool plan: %w", err)
	}
	var plan toolPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return toolPlan{}, fmt.Errorf("decode operation tool plan: %w", err)
	}
	if strings.TrimSpace(plan.Phase) == "" {
		return toolPlan{}, fmt.Errorf("operation tool plan phase is required")
	}
	if len(plan.Argv) == 0 {
		return toolPlan{}, fmt.Errorf("operation tool plan argv is required")
	}
	return plan, nil
}

func markExecuteRefused(store operation.Store, operationID string, cause error) error {
	timestamp := now()
	_, err := store.Update(operationID, "systemd-execute-refused", "systemd-execute-refused", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.NextAction = "write explicit operation run-tool plan"
		record.FailureReason = inventory.Redact(cause.Error())
		record.UpdatedAt = timestamp
		return record, nil
	})
	return err
}

func operationStoreRoot(root string) string {
	return filepath.Join(root, "var/lib/katl/operations")
}

func readCurrentBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func systemdLiveInvocation(invocation operation.InvocationRecord) bool {
	unitName := strings.TrimSpace(invocation.UnitName)
	invocationID := strings.TrimSpace(invocation.SystemdInvocationID)
	if unitName == "" || invocationID == "" {
		return false
	}
	cmd := exec.Command("systemctl", "show", unitName, "--property=ActiveState", "--property=InvocationID", "--value", "--no-pager")
	data, err := cmd.Output()
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) < 2 {
		return false
	}
	state := strings.TrimSpace(lines[0])
	activeInvocationID := strings.TrimSpace(lines[1])
	return activeInvocationID == invocationID && (state == "active" || state == "activating")
}

func digestArgv(argv []string) string {
	data, _ := json.Marshal(argv)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func generatedMarkerID(phase string, argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{phase}, argv...), "\x00")))
	return "pre-exec-" + cleanID(phase) + "-" + hex.EncodeToString(sum[:8])
}

func cleanID(value string) string {
	value = strings.Trim(strings.ToLower(value), " \t\n\r")
	value = strings.NewReplacer("_", "-", " ", "-").Replace(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "phase"
	}
	return b.String()
}

func completeInvocation(invocations []operation.InvocationRecord, id string, completedAt time.Time, result string) {
	for i := range invocations {
		if invocations[i].InvocationID == id {
			invocations[i].CompletedAt = &completedAt
			invocations[i].Result = result
			return
		}
	}
}

func exitResult(result toolResult) string {
	if result.Err != nil {
		return fmt.Sprintf("exit-%d", result.ExitStatus)
	}
	return fmt.Sprintf("exit-%d", result.ExitStatus)
}

func toolFailure(result toolResult) string {
	parts := []string{exitResult(result)}
	if len(result.Stderr) > 0 {
		parts = append(parts, strings.TrimSpace(string(result.Stderr)))
	}
	if result.Err != nil {
		parts = append(parts, result.Err.Error())
	}
	return strings.Join(parts, ": ")
}

func appendMissing(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		values = append(values, value)
		seen[value] = struct{}{}
	}
	return values
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type stringList []string

func (s *stringList) String() string {
	return strings.Join(*s, ",")
}

func (s *stringList) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("value is required")
	}
	*s = append(*s, value)
	return nil
}

func (s stringList) Values() []string {
	return append([]string(nil), s...)
}
