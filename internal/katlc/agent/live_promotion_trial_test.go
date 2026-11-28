package agent

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/efiboot"
	"github.com/katl-dev/katl/internal/generation"
)

// armTrialAfterLivePromotion arms a next-boot trial of a fresh committed
// candidate on mover, with no reboot since promoted became the default while
// another generation kept running. A trial may start only when the default is
// owned by the boot-state owner, which every live promotion must establish.
func armTrialAfterLivePromotion(t *testing.T, root string, mover *generation.BootMover, efi *fakeEFI, promoted string) {
	t.Helper()
	const candidate = "trial-candidate"

	spec, _, err := generation.ReadGeneration(root, promoted)
	if err != nil {
		t.Fatal(err)
	}
	spec.GenerationID = candidate
	spec.PreviousGenerationID = promoted
	spec.Boot.LoaderEntryPath = "loader/entries/katl-" + candidate + ".conf"
	move := func(path string) string {
		return strings.Replace(path, "/generations/"+promoted+"/", "/generations/"+candidate+"/", 1)
	}
	spec.Confexts = slices.Clone(spec.Confexts)
	spec.Confexts[0].Path = move(spec.Confexts[0].Path)
	spec.Sysexts = slices.Clone(spec.Sysexts)
	for i := range spec.Sysexts {
		spec.Sysexts[i].Path = move(spec.Sysexts[i].Path)
		writeTestFile(t, filepath.Join(root, spec.Sysexts[i].Path), "sysext")
	}
	spec.BundledConfexts = slices.Clone(spec.BundledConfexts)
	for i := range spec.BundledConfexts {
		spec.BundledConfexts[i].Path = move(spec.BundledConfexts[i].Path)
		writeTestFile(t, filepath.Join(root, spec.BundledConfexts[i].Path), "bundled confext")
	}
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStatePending, generation.HealthStateUnknown, time.Date(2026, 6, 15, 11, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	writeTestBootArtifacts(t, root, spec)

	if _, err := mover.ArmTrial(generation.TrialRequest{OperationID: "arm-after-live", GenerationID: candidate, Reason: "trial after live promotion"}); err != nil {
		t.Fatalf("ArmTrial after live promotion of %s: %v", promoted, err)
	}

	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.TrialGenerationID != candidate || selection.TargetBootGenerationID != candidate || selection.DefaultGenerationID != promoted || selection.PreviousKnownGoodGenerationID != promoted || !selection.PendingHealthValidation {
		t.Fatalf("trial selection = %+v, want trial %s over default and known good %s", selection, candidate, promoted)
	}
	want := efiboot.Entries{Default: filepath.Base(selection.DefaultBootEntry), OneShot: "katl-" + candidate + ".conf"}
	if got := efi.observed(); got != want {
		t.Fatalf("firmware boot variables = %+v, want %+v", got, want)
	}
}
