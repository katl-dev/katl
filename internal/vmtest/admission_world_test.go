package vmtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/workstationtest"
)

// useHostMemory replaces the host's memory figures, in MiB.
func useHostMemory(t *testing.T, totalMiB, availableMiB int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "meminfo")
	content := fmt.Sprintf("MemTotal:       %d kB\nMemFree:        1 kB\nMemAvailable:   %d kB\n", totalMiB*1024, availableMiB*1024)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := hostMeminfoPath
	hostMeminfoPath = path
	t.Cleanup(func() { hostMeminfoPath = previous })
}

func installedSpecs() []NodeSpec {
	return []NodeSpec{{Name: "cp-1", Role: ControlPlane}, {Name: "worker-1", Role: Worker}}
}

func TestPlanInstalledRuntimeWorldRunsAddsEverySpecUnderLease(t *testing.T) {
	world, _ := ownedScenarioWorld(t)
	writePublishedInstalledRuntimeFixture(t, world.CacheDir, "cp", "cp-1", ControlPlane, time.Unix(10, 0))
	writePublishedInstalledRuntimeFixture(t, world.CacheDir, "worker", "worker-1", Worker, time.Unix(10, 0))

	scenario, runs, err := planInstalledRuntimeWorldRuns(world, "two-nodes", installedSpecs(), KVMOff, []string{"", ""})
	if scenario != nil {
		t.Cleanup(scenario.Release)
	}

	if err != nil {
		t.Fatalf("planInstalledRuntimeWorldRuns() error = %v, want every spec added", err)
	}
	if len(runs) != 2 || runs[0].Config.FixtureManifest == "" || runs[1].Config.FixtureManifest == "" {
		t.Fatalf("runs = %+v, want one planned node per spec", runs)
	}
}

func TestPlannedScenarioReleasedAfterSetupFailure(t *testing.T) {
	world, _ := ownedScenarioWorld(t)
	t.Setenv(admissionVCPUsEnv, "2")
	t.Setenv(admissionMemoryEnv, "2048")

	// No fixture is published, so setup fails after the scenario holds its
	// reservation.
	scenario, _, err := planInstalledRuntimeWorldRuns(world, "fails", []NodeSpec{{Name: "cp-1", Role: ControlPlane}}, KVMOff, []string{""})
	if err == nil || scenario == nil {
		t.Fatalf("planInstalledRuntimeWorldRuns() = %v, %v, want the scenario with the setup error", scenario, err)
	}
	// Until it is released, the run's next scenario cannot fit beside it.
	blocked, err := world.PlanScenario("next")
	if !errors.Is(err, ErrDemandExceedsHost) {
		t.Fatalf("PlanScenario() while the failed scenario is held = %v, %v, want ErrDemandExceedsHost", blocked, err)
	}

	scenario.Release()

	next, err := world.PlanScenario("next")
	if err != nil {
		t.Fatalf("PlanScenario() after release error = %v", err)
	}
	next.Release()
}

func TestAdmissionTimeoutNamesHolders(t *testing.T) {
	a := testAdmission(t, 2, 4096)
	admitNow(t, a, "run-holder", "busy", Demand{VCPUs: 2, MemoryMiB: 4096})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := a.Admit(ctx, "run-b", "two", Demand{VCPUs: 1, MemoryMiB: 1024})

	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "run-holder") || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("Admit() error = %v, want the deadline and the holder", err)
	}
}

func TestScenarioAdmissionHonoursTimeoutEnvironment(t *testing.T) {
	world, _ := ownedScenarioWorld(t)
	t.Setenv(admissionVCPUsEnv, "2")
	t.Setenv(admissionMemoryEnv, "2048")
	world.NewScenario(t, "held")
	t.Setenv(admissionTimeoutEnv, "100ms")
	other := world
	other.RunID = "other-run"
	scenario, err := other.planScenario("waits")
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	err = scenario.admit(context.Background())

	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), world.RunID) || !strings.Contains(err.Error(), "held") {
		t.Fatalf("admit() error = %v, want the deadline naming the holder", err)
	}
	if time.Since(started) > 10*time.Second {
		t.Fatalf("admit() waited %s past a 100ms timeout", time.Since(started))
	}
	t.Setenv(admissionTimeoutEnv, "soon")
	if err := scenario.admit(context.Background()); err == nil || !strings.Contains(err.Error(), admissionTimeoutEnv) {
		t.Fatalf("admit() with a malformed timeout error = %v, want it to name %s", err, admissionTimeoutEnv)
	}
}

func TestAdmissionReapsBeforeEveryAttempt(t *testing.T) {
	a := testAdmission(t, 2, 4096)
	var reaps atomic.Int32
	a.Reap = func(context.Context) { reaps.Add(1) }

	first := admitNow(t, a, "run-a", "one", Demand{VCPUs: 2, MemoryMiB: 4096})
	first.Release()
	admitNow(t, a, "run-a", "two", Demand{VCPUs: 2, MemoryMiB: 4096})

	if got := reaps.Load(); got < 2 {
		t.Fatalf("reaps = %d, want one before each admission", got)
	}
}

func TestPlanScenarioSweepsFinishedRunsEachTime(t *testing.T) {
	world, fake := ownedScenarioWorld(t)

	for i, name := range []string{"first", "second"} {
		// A run that ended after the previous scenario was planned.
		domain := fmt.Sprintf("orphan-%d", i)
		gone := finishedLease(t, runLeaseDir(), domain)
		fake.addDomain(t, domain, vmtestMetadataFor(gone), "default")

		scenario := world.NewScenario(t, name)

		for _, remaining := range fake.objects(t, "domains") {
			if remaining == domain {
				t.Fatalf("domain %s of a finished run is still defined after planning %s", domain, name)
			}
		}
		scenario.Release()
	}
}

func TestAdmissionWaitsForAvailableMemory(t *testing.T) {
	a := testAdmission(t, 4, 65536)
	demand := Demand{VCPUs: 6, MemoryMiB: 2048}
	var available atomic.Int64
	available.Store(1024)
	a.AvailableMemoryMiB = func() (int, error) { return int(available.Load()), nil }
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := a.Admit(ctx, "run-a", "one", demand)

	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "available memory") {
		t.Fatalf("Admit() with an empty ledger and scarce host memory error = %v, want a wait on available memory", err)
	}
	available.Store(4096)
	admitNow(t, a, "run-a", "one", demand)
}

func TestAvailableMemoryLeavesHostReserve(t *testing.T) {
	// Reserve is the larger of 2 GiB and an eighth of 64 GiB.
	useHostMemory(t, 64<<10, 20<<10)

	got, err := availableMemoryMiB()
	if err != nil {
		t.Fatal(err)
	}

	if want := (20 << 10) - (8 << 10); got != want {
		t.Fatalf("availableMemoryMiB() = %d, want %d", got, want)
	}
}

func TestScenarioAdmissionCountsMemoryOfUnadmittedVMs(t *testing.T) {
	world, _ := ownedScenarioWorld(t)
	// The static budget is ample, but other VMs already use the host's memory.
	useHostMemory(t, 64<<10, 9<<10)
	t.Setenv(admissionTimeoutEnv, "100ms")
	scenario, err := world.planScenario("squeezed")
	if err != nil {
		t.Fatal(err)
	}

	err = scenario.admit(context.Background(), Nodes(4))

	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "available memory") {
		t.Fatalf("admit() error = %v, want a wait on the host's available memory", err)
	}
}

func TestEnsureWorldSSHKeyConcurrentRuns(t *testing.T) {
	workstationtest.Tool(t, "ssh-keygen")
	world := testWorld(t)
	const runs = 8
	keys := make([]string, runs)
	privates := make([]string, runs)
	errs := make([]error, runs)
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			privates[i], keys[i], errs[i] = EnsureWorldSSHKey(world)
		}()
	}
	wg.Wait()

	for i := range runs {
		if errs[i] != nil {
			t.Fatalf("EnsureWorldSSHKey() run %d error = %v", i, errs[i])
		}
		if keys[i] != keys[0] || privates[i] != privates[0] {
			t.Fatalf("run %d got key %q at %s, run 0 got %q at %s: every run must share one pair", i, keys[i], privates[i], keys[0], privates[0])
		}
	}
	derived, err := exec.Command("ssh-keygen", "-y", "-f", privates[0]).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(keys[0], strings.TrimSpace(string(derived))) {
		t.Fatalf("public key %q does not belong to the private key (derived %q)", keys[0], derived)
	}
	entries, err := os.ReadDir(filepath.Dir(privates[0]))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("ssh directory holds %d entries, want just the key pair", len(entries))
	}
}
