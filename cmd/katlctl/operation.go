package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

const operationWatchRPCDuration = 5 * time.Second

const operationPollInterval = 500 * time.Millisecond

type operationClient interface {
	GetOperation(context.Context, *agentapi.GetOperationRequest, ...grpc.CallOption) (*agentapi.OperationStatus, error)
	WatchOperation(context.Context, *agentapi.WatchOperationRequest, ...grpc.CallOption) (agentapi.KatlcAgent_WatchOperationClient, error)
}

type operationStatusOptions struct {
	endpoint       string
	agentTokenFile string
	operationID    string
	requestDigest  string
	diagnostics    string
	watch          bool
	timeout        time.Duration
	output         string
}

func newOperationCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "operation", Short: "KatlOS operation status"}
	cmd.AddCommand(newOperationStatusCommand(ctx, stdout, stderr))
	return cmd
}

func newOperationStatusCommand(ctx context.Context, stdout, stderr io.Writer) *cobra.Command {
	opts := operationStatusOptions{diagnostics: "normal", timeout: 30 * time.Minute, output: "json"}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Query or follow one accepted KatlOS operation",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runOperationStatus(ctx, opts, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "katlc agent TCP endpoint host:port")
	cmd.Flags().StringVar(&opts.agentTokenFile, "agent-token-file", "", "katlc agent bearer token file")
	cmd.Flags().StringVar(&opts.operationID, "operation-id", "", "accepted operation id")
	cmd.Flags().StringVar(&opts.requestDigest, "request-digest", "", "expected request digest returned at acceptance")
	cmd.Flags().StringVar(&opts.diagnostics, "diagnostics", opts.diagnostics, "diagnostics detail: normal or verbose")
	cmd.Flags().BoolVar(&opts.watch, "watch", false, "follow the operation until it reaches terminal state")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "overall status or watch timeout")
	cmd.Flags().StringVar(&opts.output, "output", opts.output, "output format: json")
	return cmd
}

func runOperationStatus(ctx context.Context, opts operationStatusOptions, stdout, stderr io.Writer) error {
	if opts.output != "json" {
		return fmt.Errorf("--output = %q, want json", opts.output)
	}
	if strings.TrimSpace(opts.endpoint) == "" {
		return fmt.Errorf("--endpoint is required")
	}
	if strings.TrimSpace(opts.operationID) == "" {
		return fmt.Errorf("--operation-id is required")
	}
	if opts.diagnostics != "normal" && opts.diagnostics != "verbose" {
		return fmt.Errorf("--diagnostics must be %q or %q", "normal", "verbose")
	}
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	token, err := readAgentToken(opts.agentTokenFile)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	conn, err := dialKatlcAgent(requestCtx, opts.endpoint, token)
	if err != nil {
		return err
	}
	defer conn.Close()

	request := &agentapi.GetOperationRequest{
		OperationId:           strings.TrimSpace(opts.operationID),
		ExpectedRequestDigest: strings.TrimSpace(opts.requestDigest),
		IncludeDiagnostics:    opts.diagnostics,
	}
	status, err := conn.Client.GetOperation(requestCtx, request)
	if err != nil {
		return fmt.Errorf("get operation %s: %w", request.OperationId, err)
	}
	if status == nil {
		return fmt.Errorf("agent returned an empty operation status")
	}
	if !opts.watch {
		return writeOperationStatus(stdout, status)
	}
	if status.GetTerminal() {
		if err := writeOperationStatus(stdout, status); err != nil {
			return err
		}
		return operationResultError(status)
	}

	status, err = followOperation(requestCtx, conn.Client, request, status, stderr)
	if writeErr := writeOperationStatus(stdout, status); writeErr != nil {
		return writeErr
	}
	if err != nil {
		return err
	}
	return operationResultError(status)
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
			fmt.Fprintf(stderr, "katlctl operation id=%s kind=%s phase=%s terminal=%t result=%s\n", status.GetOperationId(), status.GetOperationKind(), status.GetPhase(), status.GetTerminal(), status.GetResult())
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

func writeOperationStatus(stdout io.Writer, status *agentapi.OperationStatus) error {
	if status == nil {
		return fmt.Errorf("agent returned an empty operation status")
	}
	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal operation status: %w", err)
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
