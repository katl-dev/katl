package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const operationWatchRPCDuration = 5 * time.Second

const operationPollInterval = 500 * time.Millisecond

func clientRequestID(value string) (string, error) {
	if value = strings.TrimSpace(value); value != "" {
		return value, nil
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate request id: %w", err)
	}
	return "katlctl-" + hex.EncodeToString(random[:]), nil
}

type operationClient interface {
	GetOperation(context.Context, *agentapi.GetOperationRequest, ...grpc.CallOption) (*agentapi.OperationStatus, error)
	WatchOperation(context.Context, *agentapi.WatchOperationRequest, ...grpc.CallOption) (agentapi.KatlcAgent_WatchOperationClient, error)
}

type operationLister interface {
	ListOperations(context.Context, *agentapi.ListOperationsRequest, ...grpc.CallOption) (*agentapi.ListOperationsResponse, error)
}

type operationStatusOptions struct {
	clusterConfig string
	endpoint      string
	configPath    string
	contextName   string
	nodeName      string
	operationID   string
	diagnostics   string
	watch         bool
	timeout       time.Duration
	output        string
}

type operationListOptions struct {
	clusterConfig string
	endpoint      string
	configPath    string
	contextName   string
	nodeName      string
	activeOnly    bool
	limit         int32
	diagnostics   string
	timeout       time.Duration
	output        string
}

func newOperationCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "operations", Short: "Inspect KatlOS operations"}
	cmd.AddCommand(newOperationStatusCommand(ctx, stdout, stderr))
	cmd.AddCommand(newOperationListCommand(ctx, stdout, stderr))
	return cmd
}

func newOperationStatusCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := operationStatusOptions{diagnostics: "normal", timeout: 30 * time.Minute, output: "text"}
	cmd := &cobra.Command{
		Use:   "status [OPERATION_ID]",
		Short: "Show an active or recent KatlOS operation",
		Long:  "Show an active KatlOS operation, or the most recent operation when none is active. Supply an operation ID only when selecting a specific operation.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) == 1 {
				if strings.TrimSpace(opts.operationID) != "" {
					return fmt.Errorf("OPERATION_ID cannot be combined with --operation-id")
				}
				opts.operationID = args[0]
			}
			return runOperationStatus(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.clusterConfig, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "node address override: IP, hostname, host:port, or tcp:// URL")
	cmd.Flags().StringVar(&opts.configPath, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "optional saved context created by 'katlctl context save'")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node name from --config or a saved context; optional for one node")
	cmd.Flags().StringVar(&opts.operationID, "operation-id", "", "accepted operation id")
	cmd.Flags().Lookup("operation-id").Hidden = true
	cmd.Flags().StringVar(&opts.diagnostics, "diagnostics", opts.diagnostics, "diagnostics detail: normal or verbose")
	cmd.Flags().BoolVar(&opts.watch, "watch", false, "follow the operation until it reaches terminal state")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "overall status or watch timeout")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	return cmd
}

func newOperationListCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := operationListOptions{limit: 20, diagnostics: "normal", timeout: 15 * time.Second, output: "text"}
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List current and recent KatlOS operations",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runOperationList(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.clusterConfig, "config", "", "ClusterConfig YAML or Katl config bundle")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "node address override: IP, hostname, host:port, or tcp:// URL")
	cmd.Flags().StringVar(&opts.configPath, "context-file", "", "workstation context file path")
	cmd.Flags().Lookup("context-file").Hidden = true
	cmd.Flags().StringVar(&opts.contextName, "context", "", "optional saved context created by 'katlctl context save'")
	cmd.Flags().StringVar(&opts.nodeName, "node", "", "node name from --config or a saved context; optional for one node")
	cmd.Flags().BoolVar(&opts.activeOnly, "active", false, "show only non-terminal operations")
	cmd.Flags().Int32Var(&opts.limit, "limit", opts.limit, "maximum operations to return")
	cmd.Flags().StringVar(&opts.diagnostics, "diagnostics", opts.diagnostics, "diagnostics detail: normal or verbose")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "list request timeout")
	cmd.Flags().StringVarP(&opts.output, "output", "o", opts.output, "output format: text or json")
	return cmd
}

func runOperationList(ctx context.Context, opts operationListOptions, stdout, stderr io.Writer) error {
	_ = stderr
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	if opts.limit < 1 || opts.limit > 100 {
		return fmt.Errorf("--limit must be between 1 and 100")
	}
	if opts.diagnostics != "normal" && opts.diagnostics != "verbose" {
		return fmt.Errorf("--diagnostics must be %q or %q", "normal", "verbose")
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	target, err := resolveManagementTarget(managementTargetOptions{
		clusterConfigPath: opts.clusterConfig, configPath: opts.configPath, contextName: opts.contextName, nodeName: opts.nodeName,
		endpoint: opts.endpoint,
	})
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	conn, err := dialKatlcAgent(requestCtx, target.endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	response, err := conn.Client.ListOperations(requestCtx, &agentapi.ListOperationsRequest{
		ActiveOnly:         opts.activeOnly,
		Limit:              opts.limit,
		IncludeDiagnostics: opts.diagnostics,
	})
	if err != nil {
		return err
	}
	for _, status := range response.GetOperations() {
		status.ClientRequestId = ""
		status.RequestDigest = ""
	}
	if opts.output == "text" {
		w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tKIND\tPHASE\tRESULT\tUPDATED")
		for _, status := range response.GetOperations() {
			result := status.GetResult()
			if result == "" {
				result = "running"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", status.GetOperationId(), status.GetOperationKind(), status.GetPhase(), result, status.GetUpdatedAt())
		}
		return w.Flush()
	}
	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(response)
	if err != nil {
		return fmt.Errorf("marshal operations: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func runOperationStatus(ctx context.Context, opts operationStatusOptions, stdout, stderr io.Writer) error {
	if opts.output != "text" && opts.output != "json" {
		return fmt.Errorf("--output = %q, want text or json", opts.output)
	}
	if opts.diagnostics != "normal" && opts.diagnostics != "verbose" {
		return fmt.Errorf("--diagnostics must be %q or %q", "normal", "verbose")
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	target, err := resolveManagementTarget(managementTargetOptions{
		clusterConfigPath: opts.clusterConfig, configPath: opts.configPath, contextName: opts.contextName, nodeName: opts.nodeName,
		endpoint: opts.endpoint,
	})
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	conn, err := dialKatlcAgent(requestCtx, target.endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	operationID := strings.TrimSpace(opts.operationID)
	if operationID == "" {
		operationID, err = selectOperationID(requestCtx, conn.Client)
		if err != nil {
			return err
		}
	}

	request := &agentapi.GetOperationRequest{
		OperationId:        operationID,
		IncludeDiagnostics: opts.diagnostics,
	}
	status, err := conn.Client.GetOperation(requestCtx, request)
	if err != nil {
		return fmt.Errorf("get operation %s: %w", request.OperationId, err)
	}
	if status == nil {
		return fmt.Errorf("agent returned an empty operation status")
	}
	if !opts.watch {
		return writeOperationStatus(stdout, opts.output, status)
	}
	if status.GetTerminal() {
		if err := writeOperationStatus(stdout, opts.output, status); err != nil {
			return err
		}
		return operationResultError(status)
	}

	status, err = followOperation(requestCtx, conn.Client, request, status, stderr)
	if writeErr := writeOperationStatus(stdout, opts.output, status); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return err
	}
	return operationResultError(status)
}

func selectOperationID(ctx context.Context, client operationLister) (string, error) {
	active, err := client.ListOperations(ctx, &agentapi.ListOperationsRequest{ActiveOnly: true, Limit: 2})
	if err != nil {
		return "", fmt.Errorf("list active operations: %w", err)
	}
	operations := active.GetOperations()
	if len(operations) > 1 {
		ids := make([]string, 0, len(operations))
		for _, operation := range operations {
			ids = append(ids, operation.GetOperationId())
		}
		return "", fmt.Errorf("more than one operation is active (%s); specify the operation ID shown by 'katlctl operations list --active'", strings.Join(ids, ", "))
	}
	if len(operations) == 1 {
		return operations[0].GetOperationId(), nil
	}
	recent, err := client.ListOperations(ctx, &agentapi.ListOperationsRequest{Limit: 1})
	if err != nil {
		return "", fmt.Errorf("list recent operations: %w", err)
	}
	if len(recent.GetOperations()) == 0 {
		return "", fmt.Errorf("no operations have run on this node")
	}
	return recent.GetOperations()[0].GetOperationId(), nil
}

func followOperation(ctx context.Context, client operationClient, request *agentapi.GetOperationRequest, current *agentapi.OperationStatus, stderr io.Writer) (*agentapi.OperationStatus, error) {
	status := current
	seq := status.GetLatestJournalSeq()
	lastProgress := ""
	streamWarning := false
	watchAvailable := true
	for {
		progress := fmt.Sprintf("%s/%s/%t/%s", status.GetOperationKind(), status.GetPhase(), status.GetTerminal(), status.GetResult())
		if progress != lastProgress {
			fmt.Fprintf(stderr, "katlctl operation kind=%s phase=%s terminal=%t result=%s\n", status.GetOperationKind(), status.GetPhase(), status.GetTerminal(), status.GetResult())
			lastProgress = progress
		}
		if status.GetTerminal() {
			return status, nil
		}
		var watchErr error
		if watchAvailable {
			stream, err := client.WatchOperation(ctx, &agentapi.WatchOperationRequest{
				OperationId:           request.OperationId,
				ExpectedRequestDigest: request.ExpectedRequestDigest,
				AfterJournalSeq:       seq,
				WatchTimeout:          operationWatchRPCDuration.String(),
				IncludeDiagnostics:    request.IncludeDiagnostics,
			})
			watchErr = err
			if err == nil {
				for {
					event, recvErr := stream.Recv()
					if recvErr != nil {
						watchErr = recvErr
						break
					}
					if event.GetJournalSeq() > seq {
						seq = event.GetJournalSeq()
					}
					if event.GetStatus() != nil {
						status = event.GetStatus()
					}
					if event.GetTerminal() && status.GetTerminal() {
						return status, nil
					}
				}
			}
		}
		if watchErr != nil && !errors.Is(watchErr, io.EOF) && !errors.Is(watchErr, context.DeadlineExceeded) && !errors.Is(watchErr, context.Canceled) && !streamWarning {
			fmt.Fprintf(stderr, "katlctl operation watch interrupted; falling back to authoritative status polling: %v\n", watchErr)
			streamWarning = true
			watchAvailable = false
		}
		polled, pollErr := client.GetOperation(ctx, request)
		if pollErr != nil {
			if ctx.Err() != nil {
				return status, fmt.Errorf("watch operation %s after phase=%s: %w", request.OperationId, status.GetPhase(), ctx.Err())
			}
			return status, fmt.Errorf("poll operation %s after watch interruption: %w", request.OperationId, pollErr)
		}
		status = polled
		if status.GetLatestJournalSeq() > seq {
			seq = status.GetLatestJournalSeq()
		}
		if ctx.Err() != nil {
			return status, fmt.Errorf("watch operation %s after phase=%s: %w", request.OperationId, status.GetPhase(), ctx.Err())
		}
		if !watchAvailable {
			timer := time.NewTimer(operationPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return status, fmt.Errorf("watch operation %s after phase=%s: %w", request.OperationId, status.GetPhase(), ctx.Err())
			case <-timer.C:
			}
		}
	}
}

func writeOperationStatus(stdout io.Writer, output string, status *agentapi.OperationStatus) error {
	if status == nil {
		return fmt.Errorf("agent returned an empty operation status")
	}
	publicStatus := proto.Clone(status).(*agentapi.OperationStatus)
	publicStatus.ClientRequestId = ""
	publicStatus.RequestDigest = ""
	if output == "text" {
		result := publicStatus.GetResult()
		if result == "" {
			result = "running"
		}
		if _, err := fmt.Fprintf(stdout, "%s %s: phase=%s result=%s\n", publicStatus.GetOperationId(), publicStatus.GetOperationKind(), publicStatus.GetPhase(), result); err != nil {
			return err
		}
		if next := strings.TrimSpace(publicStatus.GetNextAction()); next != "" {
			_, err := fmt.Fprintf(stdout, "Next action: %s\n", next)
			return err
		}
		return nil
	}
	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(publicStatus)
	if err != nil {
		return fmt.Errorf("marshal operation status: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func writeMutationOperationStatus(stdout io.Writer, status *agentapi.OperationStatus) error {
	if status == nil {
		return fmt.Errorf("agent returned an empty operation status")
	}
	publicStatus := proto.Clone(status).(*agentapi.OperationStatus)
	publicStatus.OperationId = ""
	publicStatus.ClientRequestId = ""
	publicStatus.RequestDigest = ""
	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(publicStatus)
	if err != nil {
		return fmt.Errorf("marshal operation result: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func waitAcceptedOperation(ctx context.Context, client operationClient, accepted *agentapi.OperationAccepted, timeout time.Duration, stdout, stderr io.Writer) error {
	status, err := waitAcceptedOperationStatus(ctx, client, accepted, timeout, stderr)
	if writeErr := writeMutationOperationStatus(stdout, status); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return err
	}
	return operationResultError(status)
}

func waitAcceptedOperationStatus(ctx context.Context, client operationClient, accepted *agentapi.OperationAccepted, timeout time.Duration, stderr io.Writer) (*agentapi.OperationStatus, error) {
	if accepted == nil || strings.TrimSpace(accepted.GetOperationId()) == "" {
		return nil, fmt.Errorf("agent returned an empty operation acceptance")
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request := &agentapi.GetOperationRequest{OperationId: accepted.GetOperationId(), IncludeDiagnostics: "normal"}
	status := accepted.GetInitialStatus()
	if status == nil {
		var err error
		status, err = client.GetOperation(waitCtx, request)
		if err != nil {
			return nil, fmt.Errorf("get accepted operation %s: %w", accepted.GetOperationId(), err)
		}
	}
	status = proto.Clone(status).(*agentapi.OperationStatus)
	if status.OperationId == "" {
		status.OperationId = accepted.GetOperationId()
	}
	if status.OperationKind == "" {
		status.OperationKind = accepted.GetOperationKind()
	}
	var err error
	if !status.GetTerminal() {
		status, err = followOperation(waitCtx, client, request, status, stderr)
	}
	if err != nil {
		return status, err
	}
	return status, nil
}

func writeOperationAccepted(stdout io.Writer, accepted *agentapi.OperationAccepted) error {
	if accepted == nil {
		return fmt.Errorf("agent returned an empty operation acceptance")
	}
	publicAccepted := proto.Clone(accepted).(*agentapi.OperationAccepted)
	publicAccepted.RequestDigest = ""
	publicAccepted.RecordPath = ""
	if publicAccepted.InitialStatus != nil {
		publicAccepted.InitialStatus.RequestDigest = ""
	}
	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(publicAccepted)
	if err != nil {
		return fmt.Errorf("marshal operation accepted: %w", err)
	}
	_, err = stdout.Write(append(data, '\n'))
	return err
}

func operationResultError(status *agentapi.OperationStatus) error {
	if status == nil || !status.GetTerminal() || status.GetResult() == "succeeded" {
		return nil
	}
	detail := strings.TrimSpace(status.GetFailureReason())
	if detail == "" {
		detail = strings.TrimSpace(status.GetNextAction())
	}
	if detail == "" {
		detail = "inspect operation status and diagnostics"
	}
	return fmt.Errorf("operation %s finished with result %s: %s", status.GetOperationId(), status.GetResult(), detail)
}
