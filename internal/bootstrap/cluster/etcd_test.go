package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/bootstrap/readiness"
)

func TestEtcdCheckerReportsHealthyCluster(t *testing.T) {
	transport := newFakeTransport()
	addEtcdCredentialChecks(transport, readiness.CommandResult{ExitStatus: 0})
	transport.commands[commandKey("crictl", "ps", "--name", "etcd", "--state", "Running", "--quiet")] = readiness.CommandResult{ExitStatus: 0, Stdout: "etcd-container\n"}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "endpoint", "health", "--cluster", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `[{"endpoint":"https://127.0.0.1:2379","health":true}]`,
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "endpoint", "status", "--cluster", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `[{"Endpoint":"https://127.0.0.1:2379","Status":{"header":{"member_id":161},"version":"3.5.12","leader":161}}]`,
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "member", "list", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `{"members":[{"ID":"a1","name":"cp-1","peerURLs":["https://cp-1:2380"],"clientURLs":["https://cp-1:2379"]},{"ID":"b2","name":"cp-2","peerURLs":["https://cp-2:2380"],"clientURLs":["https://cp-2:2379"]},{"ID":"c3","name":"cp-3","peerURLs":["https://cp-3:2380"],"clientURLs":["https://cp-3:2379"]}]}`,
	}

	report, err := EtcdChecker{Transport: transport}.Check(context.Background(), plannedControlPlane())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !report.Healthy {
		t.Fatalf("Healthy = false, diagnostics = %#v", report.Diagnostics)
	}
	if report.ContainerID != "etcd-container" {
		t.Fatalf("ContainerID = %q", report.ContainerID)
	}
	if len(report.EndpointStatuses) != 1 || report.EndpointStatuses[0].MemberID != "a1" {
		t.Fatalf("EndpointStatuses = %#v", report.EndpointStatuses)
	}
	if len(report.Members) != 3 || report.Quorum != 2 {
		t.Fatalf("Members/Quorum = %#v/%d", report.Members, report.Quorum)
	}
}

func TestEtcdCheckerReportsUnhealthyMemberWithRedaction(t *testing.T) {
	secret := "abcdef.0123456789abcdef"
	transport := newFakeTransport()
	addEtcdCredentialChecks(transport, readiness.CommandResult{ExitStatus: 0})
	transport.commands[commandKey("crictl", "ps", "--name", "etcd", "--state", "Running", "--quiet")] = readiness.CommandResult{ExitStatus: 0, Stdout: "etcd-container\n"}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "endpoint", "health", "--cluster", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `[{"endpoint":"https://cp-2:2379","health":false,"error":"deadline exceeded with token ` + secret + `"}]`,
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "endpoint", "status", "--cluster", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `[{"Endpoint":"https://cp-1:2379","Status":{"header":{"member_id":"a1"},"version":"3.5.12","leader":"a1"}}]`,
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "member", "list", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `{"members":[{"ID":"a1","name":"cp-1"},{"ID":"b2","name":"cp-2"},{"ID":"c3","name":"cp-3"}]}`,
	}

	report, err := EtcdChecker{Transport: transport}.Check(context.Background(), plannedControlPlane())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.Healthy {
		t.Fatal("Healthy = true, want unhealthy")
	}
	got := diagnosticsText(report.Diagnostics)
	if strings.Contains(got, secret) {
		t.Fatalf("diagnostics leaked secret: %q", got)
	}
	if len(report.EndpointHealth) != 1 || strings.Contains(report.EndpointHealth[0].Error, secret) {
		t.Fatalf("structured health report leaked secret: %#v", report.EndpointHealth)
	}
	if !strings.Contains(got, "[REDACTED BOOTSTRAP TOKEN]") {
		t.Fatalf("diagnostics missing redaction marker: %q", got)
	}
	if !strings.Contains(got, "endpoint https://cp-2:2379 is unhealthy") {
		t.Fatalf("diagnostics missing unhealthy endpoint: %q", got)
	}
}

func TestEtcdCheckerReportsMissingCredentials(t *testing.T) {
	transport := newFakeTransport()
	addEtcdCredentialChecks(transport, readiness.CommandResult{ExitStatus: 0})
	transport.commands[commandKey("test", "-r", defaultEtcdClientKey)] = readiness.CommandResult{ExitStatus: 1, Stderr: "missing key Bearer secret-token"}

	report, err := EtcdChecker{Transport: transport}.Check(context.Background(), plannedControlPlane())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.Healthy {
		t.Fatal("Healthy = true, want missing credentials")
	}
	got := diagnosticsText(report.Diagnostics)
	if !strings.Contains(got, "etcd-credentials") || !strings.Contains(got, defaultEtcdClientKey) {
		t.Fatalf("diagnostics = %q, want missing credential", got)
	}
	if strings.Contains(got, "secret-token") || !strings.Contains(got, "Bearer [REDACTED]") {
		t.Fatalf("credential diagnostic was not redacted: %q", got)
	}
	if transport.commandCount(commandKey("crictl", "ps", "--name", "etcd", "--state", "Running", "--quiet")) != 0 {
		t.Fatal("etcd container lookup ran despite missing credentials")
	}
}

func TestEtcdCheckerReportsEmptyHealthOutput(t *testing.T) {
	transport := newFakeTransport()
	addEtcdCredentialChecks(transport, readiness.CommandResult{ExitStatus: 0})
	transport.commands[commandKey("crictl", "ps", "--name", "etcd", "--state", "Running", "--quiet")] = readiness.CommandResult{ExitStatus: 0, Stdout: "etcd-container\n"}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "endpoint", "health", "--cluster", "--write-out=json")...)] = readiness.CommandResult{ExitStatus: 0, Stdout: `[]`}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "endpoint", "status", "--cluster", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `[{"Endpoint":"https://cp-1:2379","Status":{"header":{"member_id":"a1"},"version":"3.5.12","leader":"a1"}}]`,
	}
	transport.commands[commandKey(etcdctlArgs("etcd-container", "member", "list", "--write-out=json")...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `{"members":[{"ID":"a1","name":"cp-1"}]}`,
	}

	report, err := EtcdChecker{Transport: transport}.Check(context.Background(), plannedControlPlane())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.Healthy {
		t.Fatal("Healthy = true, want empty health diagnostic")
	}
	if !strings.Contains(diagnosticsText(report.Diagnostics), "no endpoint health entries") {
		t.Fatalf("Diagnostics = %#v, want empty health diagnostic", report.Diagnostics)
	}
}

func TestEtcdCheckerRejectsWorkerNode(t *testing.T) {
	transport := newFakeTransport()
	node := plannedControlPlane()
	node.SystemRole = inventory.RoleWorker

	report, err := EtcdChecker{Transport: transport}.Check(context.Background(), node)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Field != "systemRole" {
		t.Fatalf("Diagnostics = %#v, want systemRole refusal", report.Diagnostics)
	}
	if len(transport.commandCalls) != 0 {
		t.Fatalf("commands ran for worker node: %#v", transport.commandCalls)
	}
}

func TestEtcdCheckerCreatesSnapshotAndReadsStatus(t *testing.T) {
	transport := newFakeTransport()
	addEtcdCredentialChecks(transport, readiness.CommandResult{ExitStatus: 0})
	transport.commands[commandKey("crictl", "ps", "--name", "etcd", "--state", "Running", "--quiet")] = readiness.CommandResult{ExitStatus: 0, Stdout: "etcd-container\n"}
	transport.commands[commandKey("install", "-d", "-m", "0700", etcdSnapshotDirectory)] = readiness.CommandResult{ExitStatus: 0}
	snapshotPath := etcdSnapshotDirectory + "/greenfield.db"
	transport.commands[commandKey(etcdctlArgs("etcd-container", "snapshot", "save", snapshotPath)...)] = readiness.CommandResult{ExitStatus: 0, Stderr: "snapshot saved"}
	transport.commands[commandKey("chmod", "0600", snapshotPath)] = readiness.CommandResult{ExitStatus: 0}
	transport.commands[commandKey(etcdutlArgs("etcd-container", "--write-out=json", "snapshot", "status", snapshotPath)...)] = readiness.CommandResult{
		ExitStatus: 0,
		Stdout:     `[{"hash":"abcd","revision":42,"totalKey":123,"totalSize":4567}]`,
	}

	report, err := EtcdChecker{Transport: transport}.CreateSnapshot(context.Background(), plannedControlPlane(), snapshotPath)
	if err != nil {
		t.Fatalf("CreateSnapshot() error = %v", err)
	}
	if len(report.Diagnostics) != 0 {
		t.Fatalf("Diagnostics = %#v", report.Diagnostics)
	}
	if report.Hash != "abcd" || report.Revision != "42" || report.TotalKeys != "123" || report.TotalSize != "4567" {
		t.Fatalf("snapshot report = %#v", report)
	}
}

func TestEtcdCheckerRejectsSnapshotOutsideRestrictedDirectory(t *testing.T) {
	transport := newFakeTransport()
	report, err := EtcdChecker{Transport: transport}.CreateSnapshot(context.Background(), plannedControlPlane(), "/tmp/etcd.db")
	if err != nil {
		t.Fatalf("CreateSnapshot() error = %v", err)
	}
	if len(report.Diagnostics) != 1 || !strings.Contains(report.Diagnostics[0].Message, etcdSnapshotDirectory) {
		t.Fatalf("Diagnostics = %#v, want restricted directory refusal", report.Diagnostics)
	}
	if len(transport.commandCalls) != 0 {
		t.Fatalf("commands ran for refused snapshot path: %#v", transport.commandCalls)
	}
}

func addEtcdCredentialChecks(transport *fakeTransport, result readiness.CommandResult) {
	for _, path := range []string{defaultEtcdCACert, defaultEtcdClientCert, defaultEtcdClientKey} {
		transport.commands[commandKey("test", "-r", path)] = result
	}
}

func etcdctlArgs(containerID string, argv ...string) []string {
	base := []string{
		"crictl", "exec", containerID, "etcdctl",
		"--endpoints=" + defaultEtcdEndpoint,
		"--cacert=" + defaultEtcdCACert,
		"--cert=" + defaultEtcdClientCert,
		"--key=" + defaultEtcdClientKey,
	}
	return append(base, argv...)
}

func etcdutlArgs(containerID string, argv ...string) []string {
	base := []string{"crictl", "exec", containerID, "etcdutl"}
	return append(base, argv...)
}

func plannedControlPlane() inventory.PlannedNode {
	return inventory.PlannedNode{
		Name:       "cp-1",
		Address:    "10.0.0.11",
		SystemRole: inventory.RoleControlPlane,
		Action:     inventory.ActionInit,
	}
}

func diagnosticsText(diagnostics []inventory.Diagnostic) string {
	var parts []string
	for _, diagnostic := range diagnostics {
		parts = append(parts, diagnostic.Field+": "+diagnostic.Message)
	}
	return strings.Join(parts, "\n")
}
