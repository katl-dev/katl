package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestCollectEtcdStatusReportsMemberIdentityAndHealth(t *testing.T) {
	report, err := collectEtcdStatus(context.Background(), fakeEtcdRunner(false))
	if err != nil {
		t.Fatal(err)
	}
	if report.GetClusterId() != "64" || report.GetLocalMemberId() != "1" || report.GetLeaderId() != "1" {
		t.Fatalf("identity = cluster %s local %s leader %s", report.GetClusterId(), report.GetLocalMemberId(), report.GetLeaderId())
	}
	if len(report.GetMembers()) != 3 || report.GetHealthyMembers() != 3 || report.GetQuorum() != 2 {
		t.Fatalf("status = %+v", report)
	}
	if report.GetMembers()[2].GetName() != "cp-3" || report.GetMembers()[2].GetId() != "3" || !report.GetMembers()[2].GetHealthy() {
		t.Fatalf("cp-3 = %+v", report.GetMembers()[2])
	}
}

func TestValidateEtcdRemovalPreconditionsAllowsOneFailedMemberWithQuorum(t *testing.T) {
	report := &agentapi.EtcdStatus{
		ClusterId:      "64",
		LocalMemberId:  "1",
		LeaderId:       "1",
		Quorum:         2,
		HealthyMembers: 2,
		Members: []*agentapi.EtcdMember{
			{Id: "1", Name: "cp-1", Healthy: true, PeerUrls: []string{"https://10.0.0.1:2380"}},
			{Id: "2", Name: "cp-2", Healthy: true, PeerUrls: []string{"https://10.0.0.2:2380"}},
			{Id: "3", Name: "cp-3", PeerUrls: []string{"https://10.0.0.3:2380"}},
		},
	}
	target, err := validateEtcdRemovalPreconditions(report, operation.EtcdMemberRemove{
		TargetNodeName: "cp-3", TargetMemberID: "3", TargetPeerURL: "https://10.0.0.3:2380",
		ExpectedClusterID: "64", ExpectedMemberCount: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if target.GetName() != "cp-3" {
		t.Fatalf("target = %+v", target)
	}
	report.HealthyMembers = 1
	if _, err := validateEtcdRemovalPreconditions(report, operation.EtcdMemberRemove{
		TargetNodeName: "cp-3", TargetMemberID: "3", TargetPeerURL: "https://10.0.0.3:2380",
		ExpectedClusterID: "64", ExpectedMemberCount: 3,
	}); err == nil || !strings.Contains(err.Error(), "below quorum") {
		t.Fatalf("unsafe removal error = %v", err)
	}
}

func TestEtcdMemberRemoveOperationValidatesAndVerifiesRemoval(t *testing.T) {
	server := newTestServer(t)
	executor := NewExecutor(server.Root, server.Store, "agent-test")
	executor.Async = false
	executor.Now = server.Now
	removed := false
	executor.RunTool = func(ctx context.Context, argv []string, started func(int)) ToolResult {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "member remove 3") {
			removed = true
			return ToolResult{Stdout: []byte("Member 3 removed")}
		}
		return fakeEtcdRunner(removed)(ctx, argv, started)
	}
	server.Dispatcher = executor

	req := submitRequest("req-etcd-remove")
	req.Bootstrap = nil
	req.OperationKind = OperationKindEtcdMemberRemove
	req.EtcdMemberRemove = &agentapi.EtcdMemberRemoveOperationRequest{
		TargetNodeName: "cp-3", TargetMemberId: "3", TargetPeerUrl: "https://10.0.0.3:2380",
		ExpectedClusterId: "64", ExpectedMemberCount: 3,
	}
	accepted, err := server.SubmitOperation(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	record, err := server.Store.Read(accepted.GetOperationId())
	if err != nil {
		t.Fatal(err)
	}
	if !removed || !record.Terminal {
		t.Fatalf("removed=%v record=%+v", removed, record)
	}
	if record.Result != operation.ResultSucceeded || !record.ExternalMutationStarted || !record.MutatingToolRan {
		t.Fatalf("record = %+v", record)
	}
}

func fakeEtcdRunner(removed bool) ToolRunner {
	return func(_ context.Context, argv []string, _ func(int)) ToolResult {
		joined := strings.Join(argv, " ")
		switch {
		case joined == "crictl ps --name etcd --state Running --quiet":
			return ToolResult{Stdout: []byte("etcd-container\n")}
		case strings.Contains(joined, "endpoint status"):
			return ToolResult{Stdout: []byte(`[{"Endpoint":"https://127.0.0.1:2379","Status":{"header":{"cluster_id":100,"member_id":1},"leader":1}}]`)}
		case strings.Contains(joined, "member list"):
			members := `{"members":[{"ID":1,"name":"cp-1","peerURLs":["https://10.0.0.1:2380"],"clientURLs":["https://10.0.0.1:2379"]},{"ID":2,"name":"cp-2","peerURLs":["https://10.0.0.2:2380"],"clientURLs":["https://10.0.0.2:2379"]}`
			if !removed {
				members += `,{"ID":3,"name":"cp-3","peerURLs":["https://10.0.0.3:2380"],"clientURLs":["https://10.0.0.3:2379"]}`
			}
			return ToolResult{Stdout: []byte(members + `]}`)}
		case strings.Contains(joined, "endpoint health"):
			return ToolResult{Stdout: []byte(`[{"health":true}]`)}
		default:
			return ToolResult{Err: fmt.Errorf("unexpected command: %s", joined), ExitStatus: 1}
		}
	}
}
