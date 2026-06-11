package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zariel/katl/internal/bootstrap/inventory"
	"github.com/zariel/katl/internal/bootstrap/readiness"
)

const (
	defaultEtcdEndpoint   = "https://127.0.0.1:2379"
	defaultEtcdCACert     = "/etc/kubernetes/pki/etcd/ca.crt"
	defaultEtcdClientCert = "/etc/kubernetes/pki/etcd/healthcheck-client.crt"
	defaultEtcdClientKey  = "/etc/kubernetes/pki/etcd/healthcheck-client.key"
	etcdSnapshotDirectory = "/var/lib/etcd/katl-snapshots"
)

type EtcdChecker struct {
	Transport      readiness.CommandTransport
	Timeout        time.Duration
	OutputLimit    uint32
	Endpoint       string
	CACertPath     string
	ClientCertPath string
	ClientKeyPath  string
}

type EtcdReport struct {
	Node             string
	Healthy          bool
	ContainerID      string
	EndpointStatuses []EtcdEndpointStatus
	EndpointHealth   []EtcdEndpointHealth
	Members          []EtcdMember
	Quorum           int
	Diagnostics      []inventory.Diagnostic
}

type EtcdEndpointStatus struct {
	Endpoint string
	MemberID string
	Version  string
	Leader   string
}

type EtcdEndpointHealth struct {
	Endpoint string
	Healthy  bool
	Error    string
}

type EtcdMember struct {
	ID         string
	Name       string
	PeerURLs   []string
	ClientURLs []string
	IsLearner  bool
}

type EtcdSnapshotReport struct {
	Node        string
	Path        string
	Hash        string
	Revision    string
	TotalKeys   string
	TotalSize   string
	Diagnostics []inventory.Diagnostic
}

func (r EtcdReport) HasMember(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, member := range r.Members {
		if member.Name == name {
			return true
		}
	}
	return false
}

func (c EtcdChecker) Check(ctx context.Context, node inventory.PlannedNode) (EtcdReport, error) {
	report := EtcdReport{Node: node.Name}
	if node.SystemRole != inventory.RoleControlPlane {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{
			Field:   "systemRole",
			Message: fmt.Sprintf("etcd checks require control-plane node, got %s", node.SystemRole),
		})
		return report, nil
	}
	if c.Transport == nil {
		return report, errors.New("etcd command transport is required")
	}
	if err := c.checkCredentials(ctx, node, &report.Diagnostics); err != nil {
		return report, err
	}
	if len(report.Diagnostics) > 0 {
		return report, nil
	}
	containerID, err := c.etcdContainerID(ctx, node)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-container", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	report.ContainerID = containerID

	health, err := c.etcdctl(ctx, node, containerID, "endpoint", "health", "--cluster", "--write-out=json")
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-health", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	report.EndpointHealth, err = parseEndpointHealth(health.Stdout)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-health", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	if len(report.EndpointHealth) == 0 {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-health", Message: "no endpoint health entries returned"})
	}
	for _, endpoint := range report.EndpointHealth {
		if !endpoint.Healthy {
			msg := "endpoint " + endpoint.Endpoint + " is unhealthy"
			if strings.TrimSpace(endpoint.Error) != "" {
				msg += ": " + endpoint.Error
			}
			report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-health", Message: msg})
		}
	}

	status, err := c.etcdctl(ctx, node, containerID, "endpoint", "status", "--cluster", "--write-out=json")
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-status", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	report.EndpointStatuses, err = parseEndpointStatus(status.Stdout)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-status", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	if len(report.EndpointStatuses) == 0 {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-status", Message: "no endpoint status entries returned"})
	}

	members, err := c.etcdctl(ctx, node, containerID, "member", "list", "--write-out=json")
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-members", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	report.Members, err = parseMembers(members.Stdout)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-members", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	if len(report.Members) == 0 {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-members", Message: "no etcd members returned"})
	}
	report.Quorum = quorum(len(report.Members))
	report.Healthy = len(report.Diagnostics) == 0
	return report, nil
}

func (c EtcdChecker) CreateSnapshot(ctx context.Context, node inventory.PlannedNode, snapshotPath string) (EtcdSnapshotReport, error) {
	report := EtcdSnapshotReport{Node: node.Name, Path: snapshotPath}
	if node.SystemRole != inventory.RoleControlPlane {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{
			Field:   "systemRole",
			Message: fmt.Sprintf("etcd snapshots require control-plane node, got %s", node.SystemRole),
		})
		return report, nil
	}
	if c.Transport == nil {
		return report, errors.New("etcd command transport is required")
	}
	path, err := c.validateSnapshotPath(snapshotPath)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-snapshot", Message: err.Error()})
		return report, nil
	}
	report.Path = path
	if err := c.checkCredentials(ctx, node, &report.Diagnostics); err != nil {
		return report, err
	}
	if len(report.Diagnostics) > 0 {
		return report, nil
	}
	containerID, err := c.etcdContainerID(ctx, node)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-container", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	if _, err := c.run(ctx, node, []string{"install", "-d", "-m", "0700", filepath.Dir(path)}, false); err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-snapshot", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	if _, err := c.etcdctl(ctx, node, containerID, "snapshot", "save", path); err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-snapshot", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	if _, err := c.run(ctx, node, []string{"chmod", "0600", path}, false); err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-snapshot", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	status, err := c.etcdutl(ctx, node, containerID, "--write-out=json", "snapshot", "status", path)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-snapshot", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	snapshot, err := parseSnapshotStatus(status.Stdout)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, inventory.Diagnostic{Field: "etcd-snapshot", Message: inventory.Redact(err.Error())})
		return report, nil
	}
	report.Hash = snapshot.Hash
	report.Revision = snapshot.Revision
	report.TotalKeys = snapshot.TotalKeys
	report.TotalSize = snapshot.TotalSize
	return report, nil
}

func (c EtcdChecker) checkCredentials(ctx context.Context, node inventory.PlannedNode, diagnostics *[]inventory.Diagnostic) error {
	for _, path := range []string{c.caCertPath(), c.clientCertPath(), c.clientKeyPath()} {
		if _, err := c.run(ctx, node, []string{"test", "-r", path}, false); err != nil {
			*diagnostics = append(*diagnostics, inventory.Diagnostic{
				Field:   "etcd-credentials",
				Message: fmt.Sprintf("required etcd credential %s is not readable: %s", path, inventory.Redact(err.Error())),
			})
		}
	}
	return nil
}

func (c EtcdChecker) etcdContainerID(ctx context.Context, node inventory.PlannedNode) (string, error) {
	result, err := c.run(ctx, node, []string{"crictl", "ps", "--name", "etcd", "--state", "Running", "--quiet"}, false)
	if err != nil {
		return "", err
	}
	for _, field := range strings.Fields(result.Stdout) {
		return field, nil
	}
	return "", errors.New("running kubeadm etcd container not found")
}

func (c EtcdChecker) etcdctl(ctx context.Context, node inventory.PlannedNode, containerID string, argv ...string) (readiness.CommandResult, error) {
	base := []string{
		"crictl", "exec", containerID, "etcdctl",
		"--endpoints=" + c.endpoint(),
		"--cacert=" + c.caCertPath(),
		"--cert=" + c.clientCertPath(),
		"--key=" + c.clientKeyPath(),
	}
	base = append(base, argv...)
	return c.run(ctx, node, base, true)
}

func (c EtcdChecker) etcdutl(ctx context.Context, node inventory.PlannedNode, containerID string, argv ...string) (readiness.CommandResult, error) {
	base := []string{"crictl", "exec", containerID, "etcdutl"}
	base = append(base, argv...)
	return c.run(ctx, node, base, true)
}

func (c EtcdChecker) run(ctx context.Context, node inventory.PlannedNode, argv []string, sensitive bool) (readiness.CommandResult, error) {
	result, err := c.Transport.RunCommand(ctx, node, readiness.CommandRequest{
		Argv:            argv,
		Timeout:         c.timeout(),
		StdoutLimit:     c.outputLimit(),
		StderrLimit:     c.outputLimit(),
		SensitiveOutput: sensitive,
	})
	if err != nil {
		return result, err
	}
	if result.ExitStatus != 0 {
		return result, commandError(argv, result)
	}
	return result, nil
}

func (c EtcdChecker) validateSnapshotPath(snapshotPath string) (string, error) {
	snapshotPath = filepath.Clean(strings.TrimSpace(snapshotPath))
	if snapshotPath == "." || !filepath.IsAbs(snapshotPath) {
		return "", errors.New("snapshot path must be absolute")
	}
	baseDir := filepath.Clean(c.snapshotBaseDir())
	relative, err := filepath.Rel(baseDir, snapshotPath)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." || filepath.IsAbs(relative) {
		return "", fmt.Errorf("snapshot path must be under %s", baseDir)
	}
	if strings.HasPrefix(filepath.Base(snapshotPath), ".") {
		return "", errors.New("snapshot filename must not be hidden")
	}
	return snapshotPath, nil
}

func (c EtcdChecker) timeout() time.Duration {
	if c.Timeout != 0 {
		return c.Timeout
	}
	return 2 * time.Minute
}

func (c EtcdChecker) outputLimit() uint32 {
	if c.OutputLimit != 0 {
		return c.OutputLimit
	}
	return 256 << 10
}

func (c EtcdChecker) endpoint() string {
	if strings.TrimSpace(c.Endpoint) != "" {
		return strings.TrimSpace(c.Endpoint)
	}
	return defaultEtcdEndpoint
}

func (c EtcdChecker) caCertPath() string {
	if strings.TrimSpace(c.CACertPath) != "" {
		return strings.TrimSpace(c.CACertPath)
	}
	return defaultEtcdCACert
}

func (c EtcdChecker) clientCertPath() string {
	if strings.TrimSpace(c.ClientCertPath) != "" {
		return strings.TrimSpace(c.ClientCertPath)
	}
	return defaultEtcdClientCert
}

func (c EtcdChecker) clientKeyPath() string {
	if strings.TrimSpace(c.ClientKeyPath) != "" {
		return strings.TrimSpace(c.ClientKeyPath)
	}
	return defaultEtcdClientKey
}

func (c EtcdChecker) snapshotBaseDir() string {
	return etcdSnapshotDirectory
}

func parseEndpointHealth(data string) ([]EtcdEndpointHealth, error) {
	var raw []struct {
		Endpoint string `json:"endpoint"`
		Health   bool   `json:"health"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, fmt.Errorf("decode endpoint health: %w", err)
	}
	health := make([]EtcdEndpointHealth, 0, len(raw))
	for _, item := range raw {
		health = append(health, EtcdEndpointHealth{Endpoint: item.Endpoint, Healthy: item.Health, Error: inventory.Redact(item.Error)})
	}
	return health, nil
}

func parseEndpointStatus(data string) ([]EtcdEndpointStatus, error) {
	var raw []struct {
		Endpoint string `json:"Endpoint"`
		Status   struct {
			Header struct {
				MemberID json.RawMessage `json:"member_id"`
			} `json:"header"`
			Version string          `json:"version"`
			Leader  json.RawMessage `json:"leader"`
		} `json:"Status"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, fmt.Errorf("decode endpoint status: %w", err)
	}
	statuses := make([]EtcdEndpointStatus, 0, len(raw))
	for _, item := range raw {
		statuses = append(statuses, EtcdEndpointStatus{
			Endpoint: item.Endpoint,
			MemberID: rawID(item.Status.Header.MemberID),
			Version:  item.Status.Version,
			Leader:   rawID(item.Status.Leader),
		})
	}
	return statuses, nil
}

func parseMembers(data string) ([]EtcdMember, error) {
	var raw struct {
		Members []struct {
			ID         json.RawMessage `json:"ID"`
			Name       string          `json:"name"`
			PeerURLs   []string        `json:"peerURLs"`
			ClientURLs []string        `json:"clientURLs"`
			IsLearner  bool            `json:"isLearner"`
		} `json:"members"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, fmt.Errorf("decode member list: %w", err)
	}
	members := make([]EtcdMember, 0, len(raw.Members))
	for _, item := range raw.Members {
		members = append(members, EtcdMember{
			ID:         rawID(item.ID),
			Name:       item.Name,
			PeerURLs:   append([]string(nil), item.PeerURLs...),
			ClientURLs: append([]string(nil), item.ClientURLs...),
			IsLearner:  item.IsLearner,
		})
	}
	return members, nil
}

func parseSnapshotStatus(data string) (EtcdSnapshotReport, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &items); err != nil {
		var item map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &item); err != nil {
			return EtcdSnapshotReport{}, fmt.Errorf("decode snapshot status: %w", err)
		}
		items = []map[string]json.RawMessage{item}
	}
	if len(items) == 0 {
		return EtcdSnapshotReport{}, errors.New("snapshot status returned no entries")
	}
	item := items[0]
	return EtcdSnapshotReport{
		Hash:      rawString(item["hash"]),
		Revision:  rawString(item["revision"]),
		TotalKeys: rawString(item["totalKey"]),
		TotalSize: rawString(item["totalSize"]),
	}, nil
}

func rawID(raw json.RawMessage) string {
	value := rawString(raw)
	if value == "" {
		return ""
	}
	if n, err := strconv.ParseUint(value, 10, 64); err == nil {
		return strconv.FormatUint(n, 16)
	}
	return value
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var number json.Number
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err == nil {
		return number.String()
	}
	var boolean bool
	if err := json.Unmarshal(raw, &boolean); err == nil {
		return strconv.FormatBool(boolean)
	}
	return strings.Trim(string(raw), `"`)
}

func quorum(members int) int {
	if members == 0 {
		return 0
	}
	return members/2 + 1
}
