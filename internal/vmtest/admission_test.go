package vmtest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testAdmission(t *testing.T, vcpus, memoryMiB int) Admission {
	t.Helper()
	return Admission{Dir: t.TempDir(), Capacity: Demand{VCPUs: vcpus, MemoryMiB: memoryMiB}, Poll: 5 * time.Millisecond}
}

func admitNow(t *testing.T, a Admission, run, scenario string, demand Demand) *Reservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reservation, err := a.Admit(ctx, run, scenario, demand)
	if err != nil {
		t.Fatalf("Admit(%s/%s, %s) error = %v, want admitted", run, scenario, demand, err)
	}
	t.Cleanup(reservation.Release)
	return reservation
}

// syncBuffer is a progress sink that the waiting goroutine writes and the
// test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAdmissionLetsFittingRunsProceedTogether(t *testing.T) {
	a := testAdmission(t, 4, 8192)

	admitNow(t, a, "run-a", "one", Demand{VCPUs: 2, MemoryMiB: 4096})
	admitNow(t, a, "run-b", "two", Demand{VCPUs: 2, MemoryMiB: 4096})
}

func TestAdmissionWaitsForCapacityThenAdmits(t *testing.T) {
	// A waiter that counted runs instead of resources would admit the third
	// scenario here, or would also hold back the second.
	tests := []struct {
		name   string
		demand Demand
	}{
		{"vCPUs exhausted", Demand{VCPUs: 2, MemoryMiB: 1024}},
		{"memory exhausted", Demand{VCPUs: 1, MemoryMiB: 4096}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := testAdmission(t, 4, 8192)
			var progress syncBuffer
			a.Progress = &progress
			first := admitNow(t, a, "run-a", "one", Demand{VCPUs: 2, MemoryMiB: 4096})
			admitNow(t, a, "run-b", "two", Demand{VCPUs: 2, MemoryMiB: 4096})

			admitted := make(chan *Reservation, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				reservation, err := a.Admit(ctx, "run-c", "three", tt.demand)
				if err != nil {
					t.Errorf("Admit() error = %v", err)
				}
				admitted <- reservation
			}()

			waitFor(t, func() bool { return strings.Contains(progress.String(), "held by run run-a") })
			select {
			case <-admitted:
				t.Fatal("scenario admitted beyond host capacity")
			default:
			}
			if got := progress.String(); !strings.Contains(got, "waiting for "+tt.demand.String()) || !strings.Contains(got, "run run-b") {
				t.Fatalf("progress = %q, want the demand and every holder named", got)
			}

			first.Release()
			select {
			case reservation := <-admitted:
				defer reservation.Release()
			case <-time.After(5 * time.Second):
				t.Fatal("scenario not admitted after capacity freed")
			}
		})
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAdmissionSerializesVCPUOvercommit(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		heldVCPUs, nextVCPUs int
	}{
		{"oversized request", 2, 6},
		{"oversized holder", 6, 2},
		{"combined demand", 3, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := testAdmission(t, 4, 13653)
			held := admitNow(t, a, "run-a", "first", Demand{VCPUs: tt.heldVCPUs, MemoryMiB: 6144})
			next := Demand{VCPUs: tt.nextVCPUs, MemoryMiB: 6144}

			assertAdmissionWaits(t, a, "run-b", "second", next)

			held.Release()
			admitNow(t, a, "run-b", "second", next)
		})
	}
}

func TestAdmissionAllowsOwnRunGrowth(t *testing.T) {
	a := testAdmission(t, 4, 13653)
	admitNow(t, a, "run-a", "outer", Demand{VCPUs: 4, MemoryMiB: 4096})

	admitNow(t, a, "run-a", "worker", Demand{VCPUs: 2, MemoryMiB: 2048})
}

func TestAdmissionGrowthWaitsForOtherRuns(t *testing.T) {
	a := testAdmission(t, 4, 13653)
	admitNow(t, a, "run-a", "outer", Demand{VCPUs: 3, MemoryMiB: 4096})
	other := admitNow(t, a, "run-b", "other", Demand{VCPUs: 1, MemoryMiB: 2048})
	growth := Demand{VCPUs: 2, MemoryMiB: 2048}

	assertAdmissionWaits(t, a, "run-a", "more", growth)

	other.Release()
	admitNow(t, a, "run-a", "more", growth)
}

func assertAdmissionWaits(t *testing.T, a Admission, run, scenario string, demand Demand) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	reservation, err := a.Admit(ctx, run, scenario, demand)
	defer reservation.Release()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Admit(%s/%s, %s) error = %v, want it to wait for capacity", run, scenario, demand, err)
	}
}

func TestAdmissionRejectsMemoryBeyondCapacity(t *testing.T) {
	t.Run("scenario", func(t *testing.T) {
		a := testAdmission(t, 4, 8192)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		reservation, err := a.Admit(ctx, "run-a", "big", Demand{VCPUs: 2, MemoryMiB: 8193})
		defer reservation.Release()

		if !errors.Is(err, ErrDemandExceedsHost) || !strings.Contains(err.Error(), admissionMemoryEnv) {
			t.Fatalf("Admit() error = %v, want ErrDemandExceedsHost and memory capacity guidance", err)
		}
	})
	t.Run("own run", func(t *testing.T) {
		a := testAdmission(t, 4, 8192)
		admitNow(t, a, "run-a", "outer", Demand{VCPUs: 2, MemoryMiB: 4096})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		reservation, err := a.Admit(ctx, "run-a", "inner", Demand{VCPUs: 2, MemoryMiB: 4097})
		defer reservation.Release()

		if !errors.Is(err, ErrDemandExceedsHost) {
			t.Fatalf("Admit() error = %v, want ErrDemandExceedsHost", err)
		}
	})
}

func TestAdmissionStopsWaitingWhenContextEnds(t *testing.T) {
	a := testAdmission(t, 2, 4096)
	admitNow(t, a, "run-a", "one", Demand{VCPUs: 2, MemoryMiB: 4096})

	assertAdmissionWaits(t, a, "run-b", "two", Demand{VCPUs: 1, MemoryMiB: 1024})
}

func TestAdmissionReleaseFreesCapacityOnce(t *testing.T) {
	a := testAdmission(t, 2, 4096)
	first := admitNow(t, a, "run-a", "one", Demand{VCPUs: 2, MemoryMiB: 4096})

	first.Release()
	first.Release()

	admitNow(t, a, "run-b", "two", Demand{VCPUs: 2, MemoryMiB: 4096})
}

// reservationHolderProcess holds a reservation until killed, like a run whose
// tests are in progress.
func TestReservationHolderProcess(t *testing.T) {
	dir := os.Getenv("KATL_RESERVATION_HOLDER")
	if dir == "" {
		return
	}
	a := Admission{Dir: dir, Capacity: Demand{VCPUs: 4, MemoryMiB: 8192}}
	if _, err := a.Admit(context.Background(), "run-killed", "held", Demand{VCPUs: 4, MemoryMiB: 8192}); err != nil {
		panic(err)
	}
	os.Stdout.WriteString("held\n")
	select {}
}

func startReservationHolder(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestReservationHolderProcess$")
	cmd.Env = append(os.Environ(), "KATL_RESERVATION_HOLDER="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("reservation holder did not start: %v", err)
	}
	return cmd
}

func TestAdmissionReclaimsReservationOfKilledHolder(t *testing.T) {
	a := testAdmission(t, 4, 8192)
	holder := startReservationHolder(t, a.Dir)
	var progress syncBuffer
	a.Progress = &progress

	// The holder is alive but has made no progress for longer than any
	// age-based expiry here would allow: it keeps its reservation.
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err := a.Admit(ctx, "run-b", "two", Demand{VCPUs: 1, MemoryMiB: 1024})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Admit() against a live holder error = %v, want it to keep waiting", err)
	}
	if !strings.Contains(progress.String(), "run run-killed") {
		t.Fatalf("progress = %q, want the live holder named", progress.String())
	}

	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()

	admitNow(t, a, "run-b", "two", Demand{VCPUs: 4, MemoryMiB: 8192})
	files, err := filepath.Glob(filepath.Join(a.Dir, "run-killed.*.reservation"))
	if err != nil || len(files) != 0 {
		t.Fatalf("killed holder's reservation files = %v (%v), want them removed", files, err)
	}
}

func TestAdmissionIgnoresReservationNobodyHolds(t *testing.T) {
	a := testAdmission(t, 2, 4096)
	leftover := filepath.Join(a.Dir, "run-gone.old.reservation")
	if err := os.WriteFile(leftover, []byte(`{"run":"run-gone","scenario":"old","vcpus":2,"memoryMiB":4096}`), 0o600); err != nil {
		t.Fatal(err)
	}

	admitNow(t, a, "run-b", "two", Demand{VCPUs: 2, MemoryMiB: 4096})

	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover reservation still exists: %v", err)
	}
}

func TestAdmissionRacingAdmitsStayWithinCapacity(t *testing.T) {
	// Each contender has its own Admission, as separate processes do, so only
	// the ledger on disk can keep them within capacity. Counting without a
	// shared lock admits more than two at once.
	dir := t.TempDir()
	var live, peak atomic.Int32
	full := make(chan struct{})
	var markFull sync.Once
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := Admission{Dir: dir, Capacity: Demand{VCPUs: 4, MemoryMiB: 8192}, Poll: time.Millisecond}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			reservation, err := a.Admit(ctx, "run-"+string(rune('a'+i)), "s", Demand{VCPUs: 2, MemoryMiB: 1024})
			if err != nil {
				t.Errorf("Admit() error = %v", err)
				return
			}
			now := live.Add(1)
			for {
				seen := peak.Load()
				if now <= seen || peak.CompareAndSwap(seen, now) {
					break
				}
			}
			// Keep the first reservation held until another fits alongside it.
			if now >= 2 {
				markFull.Do(func() { close(full) })
			}
			select {
			case <-full:
			case <-ctx.Done():
				t.Error("timed out waiting for two concurrent reservations")
			}
			time.Sleep(10 * time.Millisecond)
			live.Add(-1)
			reservation.Release()
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > 2 {
		t.Fatalf("%d scenarios held reservations at once, capacity allows 2", got)
	}
	if got := peak.Load(); got < 2 {
		t.Fatalf("peak concurrency = %d, want independent scenarios to run together", got)
	}
}

func TestHostCapacityEnvironmentOverrides(t *testing.T) {
	useHostMemory(t, 64<<10, 48<<10)
	t.Setenv(admissionVCPUsEnv, "3")
	t.Setenv(admissionMemoryEnv, "4096")

	capacity, err := HostCapacity()
	if err != nil {
		t.Fatal(err)
	}

	if capacity != (Demand{VCPUs: 3, MemoryMiB: 4096}) {
		t.Fatalf("HostCapacity() = %+v, want the overrides", capacity)
	}
}

func TestScenarioDeclaresNodesItReserves(t *testing.T) {
	world, _ := ownedScenarioWorld(t)
	scenario := world.NewScenario(t, "declared", Nodes(2))

	scenario.NewNode(t, NodeSpec{Name: "cp-1", Role: ControlPlane})
	scenario.NewNode(t, NodeSpec{Name: "worker-1", Role: Worker})
	_, err := scenario.AddNode(NodeSpec{Name: "worker-2", Role: Worker})

	if err == nil || !strings.Contains(err.Error(), "vmtest.Nodes") {
		t.Fatalf("AddNode() beyond the declared nodes error = %v, want guidance to declare more", err)
	}
}

func TestScenarioReleaseFreesReservation(t *testing.T) {
	world, _ := ownedScenarioWorld(t)
	t.Setenv(admissionVCPUsEnv, "2")
	t.Setenv(admissionMemoryEnv, "4096")
	first := world.NewScenario(t, "first")

	blocked := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		other := world
		other.RunID = "other-run"
		scenario, err := other.planScenario("second")
		if err == nil {
			err = scenario.admit(ctx)
		}
		blocked <- err
	}()
	if err := <-blocked; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second scenario error = %v, want it to wait while the first holds capacity", err)
	}

	first.Release()

	other := world
	other.RunID = "other-run"
	scenario, err := other.planScenario("second")
	if err != nil {
		t.Fatal(err)
	}
	if err := scenario.admit(context.Background()); err != nil {
		t.Fatalf("admit after release error = %v", err)
	}
	scenario.admission.Release()
}
