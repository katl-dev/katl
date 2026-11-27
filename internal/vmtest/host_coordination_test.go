package vmtest

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/workstationtest"
)

// A run's sandbox must see the host's coordination entries rather than
// private copies. Otherwise its sweep cannot see another live run's lease and
// removes that run's VMs, and concurrent runs neither share the admission
// ledger nor serialize network creation.
func TestVMTestRunSharesHostCoordination(t *testing.T) {
	repo := scriptTestRepoRoot(t)
	tmp := workstationtest.WorktreeTempDir(t, repo)
	fakeGo, _ := writeFakeGoTools(t, tmp)
	host := writeFakeHostTools(t, tmp, true)
	useHostStateDir(t)
	t.Setenv(admissionDirEnv, "")
	if err := os.Mkdir(runLeaseDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	otherLease := filepath.Join(runLeaseDir(), "other-run")
	startLeaseHolder(t, otherLease)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	unlock, err := flockFile(ctx, networkCreationLock())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// The test binary acts as the run's go test process inside the sandbox.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(tmp, "vmtest.test")
	if err := os.WriteFile(binary, data, 0o755); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(tmp, "coordination-child")
	writeExecutable(t, child, "#!/usr/bin/env bash\nexec \"$KATL_COORDINATION_BINARY\" -test.run='^TestHostCoordinationProcess$' -test.v\n")

	cmd := exec.Command(filepath.Join(repo, "scripts", "vmtest-run"), "./internal/vmtest", "-run", "^TestHostCoordination$")
	cmd.Dir = repo
	cmd.Env = appendHostEnv(os.Environ(), host,
		"KATL_VMTEST_GO="+fakeGo,
		"KATL_FAKE_GO_ARGS="+filepath.Join(tmp, "go-args.txt"),
		"KATL_FAKE_CHILD="+child,
		"KATL_COORDINATION_BINARY="+binary,
		// A name rather than a path, so that only the shared lease directory
		// can make the lease visible.
		"KATL_COORDINATION_OTHER_RUN="+filepath.Base(otherLease),
		"KATL_VMTEST_RUN_ID=run-coordination",
		"KATL_VMTEST_RUN_DIR="+filepath.Join(tmp, "run"),
		"KATL_VMTEST_REQUIRE_SCENARIO_RESULT=0",
		"TMPDIR="+tmp,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		log, _ := os.ReadFile(filepath.Join(tmp, "run", "go-test.log"))
		t.Fatalf("vmtest-run failed: %v\n%s\ngo-test.log:\n%s", err, output, log)
	}

	for _, path := range []string{
		filepath.Join(AdmissionDir(), "probe-run-coordination"),
		filepath.Join(runLeaseDir(), "probe-run-coordination"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("a write inside the run did not reach the host's coordination entry: %v", err)
		}
	}
	// Another sandbox reads a missing lease as held, so a run that deleted its
	// own would leave its leftovers to no sweep.
	if _, err := os.Stat(filepath.Join(runLeaseDir(), "run-coordination")); err != nil {
		t.Errorf("the run deleted its own lease: %v", err)
	}
}

// TestHostCoordinationProcess runs inside the sandbox of
// TestVMTestRunSharesHostCoordination.
func TestHostCoordinationProcess(t *testing.T) {
	otherRun := os.Getenv("KATL_COORDINATION_OTHER_RUN")
	if otherRun == "" {
		t.Skip("helper process for TestVMTestRunSharesHostCoordination")
	}
	ownLease := os.Getenv("KATL_VMTEST_RUN_LEASE")
	// A sandbox without the shared directory would still let a run create it.
	if err := os.MkdirAll(runLeaseDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	otherLease := filepath.Join(runLeaseDir(), otherRun)
	finishedLease := filepath.Join(runLeaseDir(), "finished-run")
	if err := os.WriteFile(finishedLease, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	fake := newFakeLibvirt(t)
	for name, lease := range map[string]string{"other": otherLease, "own": ownLease, "finished": finishedLease} {
		fake.addDomain(t, "domain-"+name, vmtestMetadataFor(lease), "net-"+name)
		fake.addNetwork(t, OwnedNetwork{Name: "net-" + name, Owner: vmtestMetadata, Lease: lease, CIDR: "10.200.1.0/24", Gateway: "10.200.1.1"})
	}
	if err := sweep(t.Context(), fake.virsh(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.objects(t, "domains"), []string{"domain-other", "domain-own"}; !slices.Equal(got, want) {
		t.Errorf("domains after sweep = %v, want %v: only the finished run's objects may go", got, want)
	}
	if got, want := fake.objects(t, "networks"), []string{"net-other", "net-own"}; !slices.Equal(got, want) {
		t.Errorf("networks after sweep = %v, want %v", got, want)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if unlock, err := flockFile(ctx, networkCreationLock()); err == nil {
		unlock()
		t.Error("took the network creation lock that the host holds, so the run sees a private copy")
	}
	for _, dir := range []string{AdmissionDir(), runLeaseDir()} {
		if err := os.WriteFile(filepath.Join(dir, "probe-"+os.Getenv("KATL_VMTEST_RUN_ID")), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
