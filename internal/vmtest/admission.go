package vmtest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Guest size that a VM without an explicit size gets. A scenario reserves this
// much for each node it declares.
const (
	defaultVMCPUs   = 2
	defaultVMRAMMiB = 2048
)

const (
	admissionDirEnv       = "KATL_VMTEST_ADMISSION_DIR"
	admissionVCPUsEnv     = "KATL_VMTEST_ADMISSION_VCPUS"
	admissionMemoryEnv    = "KATL_VMTEST_ADMISSION_MEMORY_MIB"
	admissionPollInterval = time.Second
	admissionProgressEach = 30 * time.Second
)

// ErrDemandExceedsHost reports a demand that the host's capacity can never
// meet, however many reservations end.
var ErrDemandExceedsHost = errors.New("vmtest demand exceeds host capacity")

// Demand is the vCPU and memory that a scenario's VMs need at the same time.
type Demand struct {
	VCPUs     int `json:"vcpus"`
	MemoryMiB int `json:"memoryMiB"`
}

func nodesDemand(nodes int) Demand {
	return Demand{VCPUs: nodes * defaultVMCPUs, MemoryMiB: nodes * defaultVMRAMMiB}
}

func (d Demand) String() string {
	return fmt.Sprintf("%d vCPU / %d MiB", d.VCPUs, d.MemoryMiB)
}

func (d Demand) fitsIn(capacity Demand) bool {
	return d.VCPUs <= capacity.VCPUs && d.MemoryMiB <= capacity.MemoryMiB
}

// HostCapacity returns what VMs may use together: the host's CPUs and memory
// minus a reserve for the host itself and for whatever else runs on it. The
// KATL_VMTEST_ADMISSION_VCPUS and KATL_VMTEST_ADMISSION_MEMORY_MIB variables
// override each figure.
func HostCapacity() (Demand, error) {
	capacity := Demand{VCPUs: runtime.NumCPU()}
	memory, err := hostMemoryMiB()
	if err != nil {
		return Demand{}, err
	}
	capacity.MemoryMiB = memory - max(2048, memory/8)
	for name, field := range map[string]*int{admissionVCPUsEnv: &capacity.VCPUs, admissionMemoryEnv: &capacity.MemoryMiB} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return Demand{}, fmt.Errorf("%s must be a positive integer, got %q", name, value)
			}
			*field = n
		}
	}
	return capacity, nil
}

func hostMemoryMiB() (int, error) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0, fmt.Errorf("parse MemTotal: %w", err)
			}
			return kib / 1024, nil
		}
	}
	return 0, errors.New("MemTotal missing from /proc/meminfo")
}

// AdmissionDir returns the host's ledger of reservations. Every run of the
// same user on the host shares it.
func AdmissionDir() string {
	if dir := os.Getenv(admissionDirEnv); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("katl-vmtest-admission-%d", os.Getuid()))
}

// reservationRecord is the content of a reservation file; it describes the
// holder for progress messages and carries no liveness.
type reservationRecord struct {
	Run      string `json:"run"`
	Scenario string `json:"scenario"`
	Demand
}

// Reservation is a scenario's claim on host capacity. It lasts until Release
// or until the process ends.
type Reservation struct {
	path string
	file *os.File
}

// Admission decides which scenarios may start VMs. The ledger is the set of
// reservation files in Dir: a file counts while some process holds its flock,
// and the kernel drops that lock however the holder ends, so a killed run
// frees its reservation and the next admission removes the leftover file.
type Admission struct {
	Dir      string
	Capacity Demand
	Poll     time.Duration
	Progress io.Writer
}

// Admit reserves demand for the scenario, waiting until the ledger has room.
// A demand that capacity can never meet fails at once with
// ErrDemandExceedsHost.
func (a Admission) Admit(ctx context.Context, run, scenario string, demand Demand) (*Reservation, error) {
	if demand.VCPUs <= 0 || demand.MemoryMiB <= 0 {
		return nil, fmt.Errorf("vmtest demand %s must be positive", demand)
	}
	if !demand.fitsIn(a.Capacity) {
		return nil, fmt.Errorf("%w: scenario %s needs %s, the host allows %s; lower the scenario's nodes or raise %s and %s", ErrDemandExceedsHost, scenario, demand, a.Capacity, admissionVCPUsEnv, admissionMemoryEnv)
	}
	if err := os.MkdirAll(a.Dir, 0o755); err != nil {
		return nil, err
	}
	poll := a.Poll
	if poll <= 0 {
		poll = admissionPollInterval
	}
	progress := a.Progress
	if progress == nil {
		progress = io.Discard
	}

	var announced string
	var announcedAt time.Time
	waitStart := time.Now()
	for {
		reservation, holders, err := a.tryAdmit(run, scenario, demand)
		if err != nil {
			return nil, err
		}
		if reservation != nil {
			if !announcedAt.IsZero() {
				fmt.Fprintf(progress, "vmtest admission: scenario %s admitted for %s after %s\n", scenario, demand, time.Since(waitStart).Round(time.Second))
			}
			return reservation, nil
		}
		// Announce when the holders change and otherwise periodically, so a
		// long wait stays visible without a line per poll.
		if message := fmt.Sprintf("vmtest admission: scenario %s waiting for %s; held by %s", scenario, demand, holders); message != announced || time.Since(announcedAt) >= admissionProgressEach {
			fmt.Fprintln(progress, message)
			announced, announcedAt = message, time.Now()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("vmtest admission for scenario %s: %w", scenario, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// tryAdmit takes the reservation if the ledger has room, and otherwise names
// the live holders. The ledger lock makes counting and creating one step, so
// racing admissions cannot both claim the same room.
func (a Admission) tryAdmit(run, scenario string, demand Demand) (*Reservation, string, error) {
	unlock, err := lockFile(context.Background(), filepath.Join(a.Dir, "ledger"))
	if err != nil {
		return nil, "", err
	}
	defer unlock()

	live, err := a.liveReservations()
	if err != nil {
		return nil, "", err
	}
	var used, own Demand
	for _, held := range live {
		if held.Run == run {
			own.VCPUs += held.VCPUs
			own.MemoryMiB += held.MemoryMiB
		}
		used.VCPUs += held.VCPUs
		used.MemoryMiB += held.MemoryMiB
	}
	// A run executes its tests one after another, so a scenario that starts while
	// another of its run is live is nested in it. Waiting for the outer scenario
	// to finish would never end.
	if !(Demand{VCPUs: own.VCPUs + demand.VCPUs, MemoryMiB: own.MemoryMiB + demand.MemoryMiB}).fitsIn(a.Capacity) {
		return nil, "", fmt.Errorf("%w: scenario %s needs %s while its own run holds %s of the host's %s", ErrDemandExceedsHost, scenario, demand, own, a.Capacity)
	}
	if (Demand{VCPUs: used.VCPUs + demand.VCPUs, MemoryMiB: used.MemoryMiB + demand.MemoryMiB}).fitsIn(a.Capacity) {
		reservation, err := a.reserve(run, scenario, demand)
		return reservation, "", err
	}
	holders := make([]string, 0, len(live))
	for _, held := range live {
		holders = append(holders, fmt.Sprintf("run %s (%s, %s)", held.Run, held.Scenario, held.Demand))
	}
	sort.Strings(holders)
	return nil, strings.Join(holders, ", "), nil
}

// liveReservations returns the reservations whose holder is alive and removes
// the files of holders that are gone. Callers hold the ledger lock, so no
// reservation is between creation and locking.
func (a Admission) liveReservations() ([]reservationRecord, error) {
	paths, err := filepath.Glob(filepath.Join(a.Dir, "*.reservation"))
	if err != nil {
		return nil, err
	}
	var live []reservationRecord
	for _, path := range paths {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			// Nobody holds the reservation: its run ended without releasing it.
			_ = os.Remove(path)
			_ = file.Close()
			continue
		} else if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = file.Close()
			return nil, err
		}
		var record reservationRecord
		err = json.NewDecoder(file).Decode(&record)
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("read reservation %s: %w", path, err)
		}
		live = append(live, record)
	}
	return live, nil
}

// reserve creates the reservation file, fully written and locked before the
// ledger lock is released.
func (a Admission) reserve(run, scenario string, demand Demand) (*Reservation, error) {
	path := filepath.Join(a.Dir, clean(run)+"."+clean(scenario)+".reservation")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: reservation %s is held by another process: %v", ErrScenarioPlanned, path, err)
	}
	// Truncate only once the lock is ours: the file may belong to a live holder.
	err = file.Truncate(0)
	if err == nil {
		err = json.NewEncoder(file).Encode(reservationRecord{Run: run, Scenario: scenario, Demand: demand})
	}
	if err != nil {
		_ = os.Remove(path)
		_ = file.Close()
		return nil, err
	}
	return &Reservation{path: path, file: file}, nil
}

// Release gives the reservation back. It is safe to call more than once.
func (r *Reservation) Release() {
	if r == nil || r.file == nil {
		return
	}
	_ = os.Remove(r.path)
	_ = r.file.Close()
	r.file = nil
}
