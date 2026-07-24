package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const OperationKindEtcdMemberRemove = "etcd-member-remove"

var etcdctlTLSArgs = []string{
	"--cacert=/etc/kubernetes/pki/etcd/ca.crt",
	"--cert=/etc/kubernetes/pki/etcd/healthcheck-client.crt",
	"--key=/etc/kubernetes/pki/etcd/healthcheck-client.key",
}

func (s *Server) GetEtcdStatus(ctx context.Context, _ *agentapi.GetEtcdStatusRequest) (*agentapi.EtcdStatus, error) {
	report, err := collectEtcdStatus(ctx, s.etcdRunner())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "inspect local stacked etcd: %v", err)
	}
	return report, nil
}

func (s *Server) etcdRunner() ToolRunner {
	if s.RunEtcd != nil {
		return s.RunEtcd
	}
	return runChildProcess
}

func collectEtcdStatus(ctx context.Context, run ToolRunner) (*agentapi.EtcdStatus, error) {
	if run == nil {
		return nil, errors.New("etcd command runner is required")
	}
	containerResult := run(ctx, []string{"crictl", "ps", "--name", "etcd", "--state", "Running", "--quiet"}, nil)
	if containerResult.Err != nil || containerResult.ExitStatus != 0 {
		return nil, fmt.Errorf("find running etcd container: %s", toolFailure(containerResult))
	}
	containerID := ""
	for _, field := range strings.Fields(string(containerResult.Stdout)) {
		containerID = field
		break
	}
	if containerID == "" {
		return nil, errors.New("running etcd container not found")
	}

	localStatus, err := runEtcdctl(ctx, run, containerID, "https://127.0.0.1:2379", "endpoint", "status", "--write-out=json")
	if err != nil {
		return nil, fmt.Errorf("read local endpoint status: %w", err)
	}
	var statuses []struct {
		Status struct {
			Header struct {
				ClusterID json.RawMessage `json:"cluster_id"`
				MemberID  json.RawMessage `json:"member_id"`
			} `json:"header"`
			Leader json.RawMessage `json:"leader"`
		} `json:"Status"`
	}
	if err := json.Unmarshal(localStatus.Stdout, &statuses); err != nil || len(statuses) != 1 {
		return nil, errors.New("decode local endpoint status")
	}
	clusterID, err := etcdID(statuses[0].Status.Header.ClusterID)
	if err != nil {
		return nil, fmt.Errorf("decode etcd cluster id: %w", err)
	}
	localMemberID, err := etcdID(statuses[0].Status.Header.MemberID)
	if err != nil {
		return nil, fmt.Errorf("decode local etcd member id: %w", err)
	}
	leaderID, err := etcdID(statuses[0].Status.Leader)
	if err != nil {
		return nil, fmt.Errorf("decode etcd leader id: %w", err)
	}

	memberList, err := runEtcdctl(ctx, run, containerID, "https://127.0.0.1:2379", "member", "list", "--write-out=json")
	if err != nil {
		return nil, fmt.Errorf("list etcd members: %w", err)
	}
	var rawMembers struct {
		Members []struct {
			ID         json.RawMessage `json:"ID"`
			Name       string          `json:"name"`
			PeerURLs   []string        `json:"peerURLs"`
			ClientURLs []string        `json:"clientURLs"`
		} `json:"members"`
	}
	if err := json.Unmarshal(memberList.Stdout, &rawMembers); err != nil {
		return nil, fmt.Errorf("decode etcd member list: %w", err)
	}
	if len(rawMembers.Members) == 0 {
		return nil, errors.New("etcd member list is empty")
	}
	report := &agentapi.EtcdStatus{
		ClusterId:     clusterID,
		LocalMemberId: localMemberID,
		LeaderId:      leaderID,
		Quorum:        uint32(len(rawMembers.Members)/2 + 1),
	}
	for _, raw := range rawMembers.Members {
		id, err := etcdID(raw.ID)
		if err != nil {
			return nil, fmt.Errorf("decode etcd member %q id: %w", raw.Name, err)
		}
		member := &agentapi.EtcdMember{
			Id:         id,
			Name:       strings.TrimSpace(raw.Name),
			PeerUrls:   append([]string(nil), raw.PeerURLs...),
			ClientUrls: append([]string(nil), raw.ClientURLs...),
			Leader:     id == leaderID,
		}
		if len(member.ClientUrls) == 0 {
			member.HealthError = "member has no client URL"
		} else {
			health, healthErr := runEtcdctl(ctx, run, containerID, member.ClientUrls[0], "endpoint", "health", "--write-out=json")
			if healthErr != nil {
				member.HealthError = inventory.Redact(healthErr.Error())
			} else {
				var entries []struct {
					Health bool   `json:"health"`
					Error  string `json:"error"`
				}
				if err := json.Unmarshal(health.Stdout, &entries); err != nil || len(entries) != 1 {
					member.HealthError = "invalid endpoint health response"
				} else {
					member.Healthy = entries[0].Health
					member.HealthError = inventory.Redact(entries[0].Error)
				}
			}
		}
		if member.Healthy {
			report.HealthyMembers++
		}
		report.Members = append(report.Members, member)
	}
	return report, nil
}

func runEtcdctl(ctx context.Context, run ToolRunner, containerID, endpoint string, argv ...string) (ToolResult, error) {
	command := []string{"crictl", "exec", containerID, "etcdctl", "--endpoints=" + endpoint}
	command = append(command, etcdctlTLSArgs...)
	command = append(command, argv...)
	result := run(ctx, command, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return result, fmt.Errorf("%s", toolFailure(result))
	}
	return result, nil
}

func etcdID(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", errors.New("id is empty")
	}
	var text string
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", err
		}
	} else {
		text = string(raw)
	}
	value, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(value, 16), nil
}

func etcdMemberRemoveFromProto(req *agentapi.EtcdMemberRemoveOperationRequest) operation.EtcdMemberRemove {
	if req == nil {
		return operation.EtcdMemberRemove{}
	}
	return operation.EtcdMemberRemove{
		TargetNodeName:      strings.TrimSpace(req.TargetNodeName),
		TargetMemberID:      strings.ToLower(strings.TrimSpace(req.TargetMemberId)),
		TargetPeerURL:       strings.TrimSpace(req.TargetPeerUrl),
		ExpectedClusterID:   strings.ToLower(strings.TrimSpace(req.ExpectedClusterId)),
		ExpectedMemberCount: req.ExpectedMemberCount,
	}
}

func validateEtcdMemberRemoveRequest(kind string, req *agentapi.EtcdMemberRemoveOperationRequest) error {
	if kind != OperationKindEtcdMemberRemove {
		return fmt.Errorf("operationKind must be %q", OperationKindEtcdMemberRemove)
	}
	return operation.ValidateEtcdMemberRemove(etcdMemberRemoveFromProto(req))
}

func (e *Executor) executeEtcdMemberRemove(ctx context.Context, record operation.OperationRecord) error {
	request := record.EtcdMemberRemoveRequest
	if request == nil {
		err := errors.New("etcd member remove request is required")
		_, markErr := e.failRecordPhase(record.OperationID, "etcd-member-remove-missing-request", "etcd-member-remove-invalid", "preflight-etcd-member-remove", "resubmit a valid etcd member removal", err)
		return errors.Join(err, markErr)
	}
	if err := operation.ValidateEtcdMemberRemove(*request); err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "etcd-member-remove-invalid", "etcd-member-remove-invalid", "preflight-etcd-member-remove", "resubmit a valid etcd member removal", err)
		return errors.Join(err, markErr)
	}
	report, err := collectEtcdStatus(ctx, e.toolRunner())
	if err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "etcd-member-remove-preflight-failed", "etcd-member-remove-preflight", "preflight-etcd-member-remove", "restore etcd health before removing a member", err)
		return errors.Join(err, markErr)
	}
	target, err := validateEtcdRemovalPreconditions(report, *request)
	if err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "etcd-member-remove-refused", "etcd-member-remove-preflight", "preflight-etcd-member-remove", "inspect current etcd membership and resubmit the observed identity", err)
		return errors.Join(err, markErr)
	}
	containerResult := e.toolRunner()(ctx, []string{"crictl", "ps", "--name", "etcd", "--state", "Running", "--quiet"}, nil)
	containerFields := strings.Fields(string(containerResult.Stdout))
	if containerResult.Err != nil || containerResult.ExitStatus != 0 || len(containerFields) == 0 {
		err := fmt.Errorf("find running etcd container after preflight: %s", toolFailure(containerResult))
		_, markErr := e.failRecordPhase(record.OperationID, "etcd-container-disappeared", "etcd-member-remove-preflight", "preflight-etcd-member-remove", "restore local etcd health before removing a member", err)
		return errors.Join(err, markErr)
	}
	containerID := containerFields[0]

	now := e.clock()
	record, err = e.Store.Update(record.OperationID, "etcd-member-remove-start", "etcd-member-remove", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = "remove-etcd-member"
		record.ExternalMutationStarted = true
		record.MutationScopes = appendMissing(record.MutationScopes, "etcd-state")
		record.NextAction = "remove only the validated etcd member"
		record.UpdatedAt = now
		return record, nil
	})
	if err != nil {
		return err
	}
	if target.GetLeader() {
		if _, err := runEtcdctl(ctx, e.toolRunner(), containerID, "https://127.0.0.1:2379", "move-leader", report.GetLocalMemberId()); err != nil {
			_, markErr := e.failRecordPhase(record.OperationID, "etcd-leader-transfer-failed", "etcd-member-remove", "remove-etcd-member", "restore etcd health before retrying member removal", fmt.Errorf("move leadership to coordinator: %w", err))
			return errors.Join(err, markErr)
		}
	}
	if _, err := runEtcdctl(ctx, e.toolRunner(), containerID, "https://127.0.0.1:2379", "member", "remove", request.TargetMemberID); err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "etcd-member-remove-failed", "etcd-member-remove", "remove-etcd-member", "inspect current membership before deciding whether removal must be retried", err)
		return errors.Join(err, markErr)
	}
	post, err := collectEtcdStatus(ctx, e.toolRunner())
	if err != nil {
		_, markErr := e.failRecordPhase(record.OperationID, "etcd-member-remove-postflight-failed", "etcd-member-remove-postflight", "verify-etcd-member-remove", "inspect etcd membership from a healthy control plane", err)
		return errors.Join(err, markErr)
	}
	if post.GetClusterId() != request.ExpectedClusterID || len(post.GetMembers()) != int(request.ExpectedMemberCount)-1 || findEtcdMember(post, request.TargetMemberID) != nil || post.GetHealthyMembers() < post.GetQuorum() {
		err := errors.New("post-removal etcd membership did not match the expected healthy cluster")
		_, markErr := e.failRecordPhase(record.OperationID, "etcd-member-remove-postflight-refused", "etcd-member-remove-postflight", "verify-etcd-member-remove", "inspect etcd membership from a healthy control plane", err)
		return errors.Join(err, markErr)
	}
	completed := e.clock()
	_, err = e.Store.Update(record.OperationID, "etcd-member-remove-complete", "operation-complete", func(record operation.OperationRecord) (operation.OperationRecord, error) {
		record.Phase = operation.HostBookkeepingCompletionPhase
		record.CompletedPhases = appendMissing(record.CompletedPhases, "accepted", "preflight-etcd-member-remove", "remove-etcd-member", "verify-etcd-member-remove", operation.HostBookkeepingCompletionPhase)
		record.PhaseIndex = len(record.CompletedPhases)
		record.MutatingToolRan = true
		record.MutatingToolInvocations = appendMissing(record.MutatingToolInvocations, "etcdctl member remove")
		record.Terminal = true
		record.Result = operation.ResultSucceeded
		record.NextAction = "the removed control-plane node can now be reset or replaced"
		record.CompletedAt = &completed
		record.UpdatedAt = completed
		return record, nil
	})
	return err
}

func validateEtcdRemovalPreconditions(report *agentapi.EtcdStatus, request operation.EtcdMemberRemove) (*agentapi.EtcdMember, error) {
	if report.GetClusterId() != request.ExpectedClusterID {
		return nil, fmt.Errorf("etcd cluster id %s does not match expected %s", report.GetClusterId(), request.ExpectedClusterID)
	}
	if len(report.GetMembers()) != int(request.ExpectedMemberCount) {
		return nil, fmt.Errorf("etcd member count %d does not match expected %d", len(report.GetMembers()), request.ExpectedMemberCount)
	}
	target := findEtcdMember(report, request.TargetMemberID)
	if target == nil || target.GetName() != request.TargetNodeName || !containsText(target.GetPeerUrls(), request.TargetPeerURL) {
		return nil, errors.New("target member id, node name, and peer URL do not identify the same current etcd member")
	}
	if target.GetId() == report.GetLocalMemberId() {
		return nil, errors.New("coordinator cannot remove its own local etcd member")
	}
	if report.GetHealthyMembers() < report.GetQuorum() {
		return nil, fmt.Errorf("current etcd cluster has %d healthy members, below quorum %d", report.GetHealthyMembers(), report.GetQuorum())
	}
	healthySurvivors := report.GetHealthyMembers()
	if target.GetHealthy() {
		healthySurvivors--
	}
	postQuorum := uint32((len(report.GetMembers())-1)/2 + 1)
	if healthySurvivors < postQuorum {
		return nil, fmt.Errorf("removal would leave %d healthy members, below resulting quorum %d", healthySurvivors, postQuorum)
	}
	if len(report.GetMembers()) <= 1 {
		return nil, errors.New("cannot remove the only etcd member")
	}
	return target, nil
}

func findEtcdMember(report *agentapi.EtcdStatus, id string) *agentapi.EtcdMember {
	for _, member := range report.GetMembers() {
		if strings.EqualFold(member.GetId(), strings.TrimSpace(id)) {
			return member
		}
	}
	return nil
}

func containsText(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func validateEtcdPeerURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("invalid etcd peer URL")
	}
	return nil
}
