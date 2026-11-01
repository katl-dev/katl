package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/operation"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

// Requests to individual endpoints must stay pinned: transparent failover would
// turn a healthy peer's response into false evidence about an unavailable voter.
func observeUpgradeEtcd(ctx context.Context, root string, previous *operation.EtcdObservation) (*operation.EtcdObservation, error) {
	report := &operation.EtcdObservation{ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	tlsConfig, err := (transport.TLSInfo{
		CertFile:      rootedRuntimePath(root, "/etc/kubernetes/pki/etcd/healthcheck-client.crt"),
		KeyFile:       rootedRuntimePath(root, "/etc/kubernetes/pki/etcd/healthcheck-client.key"),
		TrustedCAFile: rootedRuntimePath(root, "/etc/kubernetes/pki/etcd/ca.crt"),
	}).ClientConfig()
	if err != nil {
		return report, err
	}
	endpoints := []string{"https://127.0.0.1:2379"}
	if previous != nil {
		report.LocalMemberID = previous.LocalMemberID
		for _, member := range previous.Members {
			endpoints = append(endpoints, member.Endpoint)
		}
	}
	client, err := clientv3.New(clientv3.Config{Endpoints: endpoints, TLS: tlsConfig, DialTimeout: 5 * time.Second, Logger: zap.NewNop()})
	if err != nil {
		return report, err
	}
	defer client.Close()
	if report.LocalMemberID == "" {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		local, err := client.Status(probeCtx, endpoints[0])
		cancel()
		if err != nil {
			return report, fmt.Errorf("identify local etcd member: %w", err)
		}
		report.LocalMemberID = strconv.FormatUint(local.Header.MemberId, 16)
	}
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	membership, err := client.MemberList(listCtx)
	cancel()
	if err != nil {
		return report, fmt.Errorf("read etcd membership: %w", err)
	}
	report.ClusterID = strconv.FormatUint(membership.Header.ClusterId, 16)
	for _, member := range membership.Members {
		observed := operation.EtcdMemberObservation{ID: strconv.FormatUint(member.ID, 16), Name: member.Name, PeerURLs: member.PeerURLs, Learner: member.IsLearner}
		if len(member.ClientURLs) > 0 {
			observed.Endpoint = member.ClientURLs[0]
		}
		report.Members = append(report.Members, observed)
	}
	slices.SortFunc(report.Members, func(a, b operation.EtcdMemberObservation) int { return strings.Compare(a.ID, b.ID) })
	// Capture a committed Raft index, then require every member to have applied
	// at least that index. Exact equality to a moving head cannot converge under load.
	for _, member := range report.Members {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, err := client.Status(probeCtx, member.Endpoint)
		cancel()
		if err == nil && status.Header.ClusterId == membership.Header.ClusterId && status.Header.MemberId == status.Leader && status.Leader != 0 {
			report.LeaderID = strconv.FormatUint(status.Leader, 16)
			report.BarrierIndex = status.RaftIndex
			break
		}
	}
	var failures []error
	for i := range report.Members {
		member := &report.Members[i]
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := observeUpgradeMember(probeCtx, tlsConfig, report, member)
		cancel()
		if err != nil {
			member.Error = err.Error()
			failures = append(failures, fmt.Errorf("member %s: %w", member.Name, err))
		}
	}
	return report, errors.Join(failures...)
}

func observeUpgradeMember(ctx context.Context, tlsConfig *tls.Config, report *operation.EtcdObservation, member *operation.EtcdMemberObservation) error {
	if member.Endpoint == "" {
		return errors.New("no client endpoint")
	}
	client, err := clientv3.New(clientv3.Config{Endpoints: []string{member.Endpoint}, TLS: tlsConfig, DialTimeout: 5 * time.Second, Logger: zap.NewNop()})
	if err != nil {
		return err
	}
	defer client.Close()
	// A default Get is linearizable, so success establishes contact with quorum.
	read, err := client.Get(ctx, "/katl/upgrade-health")
	if err != nil {
		return fmt.Errorf("linearizable read: %w", err)
	}
	status, err := client.Status(ctx, member.Endpoint)
	if err != nil {
		return err
	}
	member.Version = status.Version
	member.StorageVersion = status.StorageVersion
	member.DowngradeEnabled = status.DowngradeInfo.GetEnabled()
	member.DowngradeTarget = status.DowngradeInfo.GetTargetVersion()
	if member.StorageVersion == "" {
		member.StorageVersion = "unreported"
	}
	member.RaftTerm = status.RaftTerm
	member.AppliedIndex = status.RaftAppliedIndex
	member.Revision = status.Header.Revision
	if strconv.FormatUint(status.Header.ClusterId, 16) != report.ClusterID || strconv.FormatUint(read.Header.ClusterId, 16) != report.ClusterID || strconv.FormatUint(status.Header.MemberId, 16) != member.ID || strconv.FormatUint(read.Header.MemberId, 16) != member.ID {
		return errors.New("endpoint identity does not match membership")
	}
	if len(status.Errors) > 0 || member.DowngradeEnabled || status.IsLearner != member.Learner || strconv.FormatUint(status.Leader, 16) != report.LeaderID || report.BarrierIndex == 0 || status.RaftAppliedIndex < report.BarrierIndex {
		return fmt.Errorf("not caught up to leader %s at index %d (applied %d, errors %v)", report.LeaderID, report.BarrierIndex, status.RaftAppliedIndex, status.Errors)
	}
	alarms, err := client.AlarmList(ctx)
	if err != nil {
		return err
	}
	if len(alarms.Alarms) > 0 {
		return fmt.Errorf("etcd alarms: %v", alarms.Alarms)
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(member.Endpoint, "/")+"/version", nil)
	if err != nil {
		return err
	}
	response, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var versions struct {
		Server  string `json:"etcdserver"`
		Cluster string `json:"etcdcluster"`
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("etcd /version: HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&versions); err != nil {
		return err
	}
	member.ClusterVersion = versions.Cluster
	if versions.Server != member.Version || versions.Cluster == "" {
		return errors.New("incomplete or inconsistent etcd versions")
	}
	member.Healthy = true
	return nil
}

func validateUpgradeEtcd(observed, previous *operation.EtcdObservation, restarting bool) error {
	if observed == nil || observed.ClusterID == "" || observed.LocalMemberID == "" || observed.LeaderID == "" || observed.BarrierIndex == 0 || len(observed.Members) == 0 {
		return errors.New("incomplete direct etcd evidence")
	}
	if previous != nil {
		if observed.ClusterID != previous.ClusterID || observed.LocalMemberID != previous.LocalMemberID || len(observed.Members) != len(previous.Members) {
			return errors.New("etcd cluster or local member identity changed")
		}
		for _, before := range previous.Members {
			found := false
			for _, after := range observed.Members {
				if before.ID == after.ID && before.Name == after.Name && reflect.DeepEqual(before.PeerURLs, after.PeerURLs) && before.Endpoint == after.Endpoint {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("etcd membership changed for %s (%s)", before.Name, before.ID)
			}
		}
	}
	survivors, local := 0, false
	for _, member := range observed.Members {
		if member.Learner || member.DowngradeEnabled || !member.Healthy || member.AppliedIndex < observed.BarrierIndex || member.Version == "" || member.ClusterVersion == "" || member.StorageVersion == "" || member.Error != "" {
			return fmt.Errorf("etcd member %s is not a healthy caught-up voter: %s", member.Name, member.Error)
		}
		if member.ID == observed.LocalMemberID {
			local = true
		} else {
			survivors++
		}
	}
	if !local {
		return errors.New("local etcd member is absent from membership")
	}
	// Single-control-plane installations explicitly have no availability guarantee.
	if restarting && len(observed.Members) > 1 && survivors < len(observed.Members)/2+1 {
		return fmt.Errorf("restarting this voter leaves %d voters, below quorum %d", survivors, len(observed.Members)/2+1)
	}
	return nil
}

func etcdEvidenceProto(evidence *operation.EtcdUpgradeEvidence) *agentapi.EtcdUpgradeEvidence {
	if evidence == nil {
		return nil
	}
	out := &agentapi.EtcdUpgradeEvidence{SourceImage: evidence.SourceImage, TargetImage: evidence.TargetImage, ImageChanges: evidence.ImageChanges, Before: etcdObservationProto(evidence.Before), PreRestart: etcdObservationProto(evidence.PreRestart), After: etcdObservationProto(evidence.After), Failure: evidence.Failure}
	if s := evidence.Snapshot; s != nil {
		out.Snapshot = &agentapi.EtcdSnapshot{Path: s.Path, Sha256: s.SHA256, Revision: s.Revision, Hash: s.Hash, TotalKeys: s.TotalKeys, CreatedAt: s.CreatedAt}
	}
	return out
}

func etcdObservationProto(observed *operation.EtcdObservation) *agentapi.EtcdObservation {
	if observed == nil {
		return nil
	}
	out := &agentapi.EtcdObservation{ObservedAt: observed.ObservedAt, ClusterId: observed.ClusterID, LocalMemberId: observed.LocalMemberID, LeaderId: observed.LeaderID, BarrierIndex: observed.BarrierIndex}
	out.LocalImage = observed.LocalImage
	out.LocalContainerId = observed.LocalContainerID
	for _, m := range observed.Members {
		out.Members = append(out.Members, &agentapi.EtcdMemberObservation{Id: m.ID, Name: m.Name, Endpoint: m.Endpoint, PeerUrls: slices.Clone(m.PeerURLs), Learner: m.Learner, Healthy: m.Healthy, Version: m.Version, ClusterVersion: m.ClusterVersion, StorageVersion: m.StorageVersion, RaftTerm: m.RaftTerm, AppliedIndex: m.AppliedIndex, Revision: m.Revision, Error: m.Error})
		out.Members[len(out.Members)-1].DowngradeEnabled = m.DowngradeEnabled
		out.Members[len(out.Members)-1].DowngradeTarget = m.DowngradeTarget
	}
	return out
}

func (e *Executor) observeEtcd(ctx context.Context, previous *operation.EtcdObservation) (*operation.EtcdObservation, error) {
	if e.observeUpgradeEtcd != nil {
		return e.observeUpgradeEtcd(ctx, previous)
	}
	beforeImage, beforeID, beforeErr := e.runningEtcdImage(ctx)
	observed, err := observeUpgradeEtcd(ctx, e.Root, previous)
	if observed != nil {
		image, containerID, inspectErr := e.runningEtcdImage(ctx)
		observed.LocalImage = image
		observed.LocalContainerID = containerID
		err = errors.Join(err, beforeErr, inspectErr)
		if image != beforeImage || containerID != beforeID {
			err = errors.Join(err, errors.New("etcd container changed during direct observation"))
		}
	}
	return observed, err
}

func (e *Executor) checkUpgradeEtcd(ctx context.Context, record operation.OperationRecord, returning bool) error {
	latest, err := e.Store.Read(record.OperationID)
	if err != nil {
		return err
	}
	evidence := latest.KubeadmUpgradeEvidence.Etcd
	if evidence == nil || evidence.Before == nil {
		return errors.New("no prepared etcd evidence")
	}
	observed, inspectErr := e.observeEtcd(ctx, evidence.Before)
	err = errors.Join(inspectErr, validateUpgradeEtcd(observed, evidence.Before, !returning))
	if err == nil && returning && (observed.LocalImage != evidence.TargetImage || observed.LocalContainerID == "" || evidence.ImageChanges && observed.LocalContainerID == evidence.Before.LocalContainerID) {
		err = errors.New("target etcd container image and restart have not been observed")
	}

	_, storeErr := e.Store.Update(record.OperationID, "etcd-observed", "etcd-observed", func(current operation.OperationRecord) (operation.OperationRecord, error) {
		if returning {
			current.KubeadmUpgradeEvidence.Etcd.After = observed
		} else {
			current.KubeadmUpgradeEvidence.Etcd.PreRestart = observed
		}
		if err != nil {
			current.KubeadmUpgradeEvidence.Etcd.Failure = err.Error()
		} else {
			current.KubeadmUpgradeEvidence.Etcd.Failure = ""
		}
		return current, nil
	})
	return errors.Join(err, storeErr)
}

func (e *Executor) waitUpgradeEtcd(ctx context.Context, record operation.OperationRecord) error {
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err := e.checkUpgradeEtcd(waitCtx, record, true)
		if err == nil {
			return nil
		}
		if waitErr := waitForContext(waitCtx, 2*time.Second); waitErr != nil {
			return errors.Join(err, waitErr)
		}
	}
}

func (e *Executor) runningEtcdImage(ctx context.Context) (string, string, error) {
	result := e.toolRunner()(ctx, []string{"crictl", "ps", "--state", "Running", "--name", "etcd", "-q"}, nil)
	ids := strings.Fields(string(result.Stdout))
	if result.Err != nil || result.ExitStatus != 0 || len(ids) != 1 {
		return "", "", fmt.Errorf("identify running etcd container: %s", toolFailure(result))
	}
	result = e.toolRunner()(ctx, []string{"crictl", "inspect", ids[0]}, nil)
	if result.Err != nil || result.ExitStatus != 0 {
		return "", ids[0], fmt.Errorf("inspect running etcd image: %s", toolFailure(result))
	}
	var container struct {
		Status struct {
			Image struct {
				Image string `json:"image"`
			} `json:"image"`
		} `json:"status"`
	}
	if err := json.Unmarshal(result.Stdout, &container); err != nil {
		return "", ids[0], err
	}
	if container.Status.Image.Image == "" {
		return "", ids[0], errors.New("running etcd container has no image reference")
	}
	return container.Status.Image.Image, ids[0], nil
}
