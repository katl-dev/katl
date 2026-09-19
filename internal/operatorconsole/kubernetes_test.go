package operatorconsole

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestObservedCluster(t *testing.T) {
	data := []byte(`{"kind":"NodeList","items":[
 {"metadata":{"name":"cp-1"},"status":{"nodeInfo":{"kubeletVersion":"v1.36.4"},"conditions":[{"type":"Ready","status":"False"}]}},
 {"metadata":{"name":"cp-2"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"name":"worker-1"},"status":{"conditions":[{"type":"Ready","status":"Unknown"}]}}
 ]}`)
	got, err := decodeCluster(data, "cp-1")
	want := clusterStatus{Known: true, Nodes: 3, Ready: 1, KubeletVersion: "v1.36.4"}
	if err != nil || got != want {
		t.Fatalf("cluster = %+v, %v", got, err)
	}
	for _, input := range []string{`{}`, `null`, `{"kind":"Status","items":[]}`, `{"kind":"NodeList"}`, `broken`} {
		if got, err := decodeCluster([]byte(input), "cp-1"); err == nil || got.Known {
			t.Fatalf("invalid response %s produced %+v, %v", input, got, err)
		}
	}
	got, err = decodeCluster([]byte(`{"kind":"NodeList","items":[]}`), "cp-1")
	if err != nil || !got.Known || got.Nodes != 0 {
		t.Fatalf("empty list = %+v, %v", got, err)
	}
}

func TestObservedComponentVersions(t *testing.T) {
	pods, err := decodeControlPlanePods([]byte(`{"containers":[
 {"metadata":{"name":"kube-apiserver"},"state":"CONTAINER_RUNNING","createdAt":"20","image":{"image":"sha256:b665198f31ae0e1bb7488858964d6f2a60e6052c1091d3e79f37108b29edbcc4","userSpecifiedImage":"registry.k8s.io/kube-apiserver:v1.36.4"}},
 {"metadata":{"name":"kube-apiserver"},"state":"CONTAINER_EXITED","createdAt":"10","image":{"image":"registry.k8s.io/kube-apiserver:v1.36.3"}},
 {"metadata":{"name":"etcd"},"state":"CONTAINER_RUNNING","image":{"image":"registry.k8s.io/etcd:3.6.8-0"}}
 ]}`))
	if err != nil {
		t.Fatal(err)
	}
	if pods[0].Version != "v1.36.4" || pods[0].State != KubernetesPodRunning || pods[3].Version != "3.6.8-0" {
		t.Fatalf("pods = %+v", pods)
	}
}

func TestBootstrapConsoleVersions(t *testing.T) {
	snapshot := Snapshot{
		Mode: ModeRuntime, KubernetesConfigured: true, ControlPlane: true, Hostname: "cp-1",
		Cluster:          clusterStatus{Known: true, Nodes: 3, Ready: 2, KubeletVersion: "v1.36.4", KubeletReady: true, APIReady: true},
		ControlPlanePods: initialControlPlanePods(KubernetesPodRunning),
	}
	snapshot.ControlPlanePods[0].Version = "v1.36.4"
	snapshot.ControlPlanePods[0].State = KubernetesPodHealthy
	for _, width := range []int{80, 160} {
		output := string(renderDashboard(&snapshot, testJournal{}, width, 35, false))
		for _, want := range []string{"Kubernetes · 2/3 nodes ready", "v1.36.4 (Healthy)"} {
			if !strings.Contains(output, want) {
				t.Fatalf("missing %q:\n%s", want, output)
			}
		}
		if strings.Contains(output, "Not installed") {
			t.Fatalf("running bootstrap shown as not installed:\n%s", output)
		}
	}
	snapshot.Cluster = clusterStatus{}
	output := string(renderDashboard(&snapshot, testJournal{}, 160, 35, false))
	if strings.Contains(output, "nodes ready") {
		t.Fatalf("unknown cluster has a count:\n%s", output)
	}
}

func TestClusterObservationExpires(t *testing.T) {
	now := time.Now()
	broken := false
	collector := Collector{
		Root: t.TempDir(), Now: func() time.Time { return now },
		ProbeControlPlanePods: func(context.Context) (ControlPlanePodStatuses, error) {
			return initialControlPlanePods(KubernetesPodRunning), nil
		},
		probeCluster: func(context.Context, string) (clusterStatus, error) {
			result := clusterStatus{Known: true, Nodes: 3, Ready: 3, APIReady: true}
			if broken {
				return result, errors.New("API unavailable")
			}
			return result, nil
		},
	}
	snapshot := Snapshot{KubernetesConfigured: true, ControlPlane: true}
	collector.collectKubernetes(&snapshot, now)
	if !snapshot.Cluster.Known || snapshot.ControlPlanePods[0].State != KubernetesPodHealthy {
		t.Fatalf("healthy = %+v", snapshot)
	}
	broken = true
	now = now.Add(time.Minute)
	collector.collectKubernetes(&snapshot, now)
	if snapshot.Cluster.Known || snapshot.ControlPlanePods[0].State == KubernetesPodHealthy {
		t.Fatalf("stale health = %+v", snapshot)
	}
}
