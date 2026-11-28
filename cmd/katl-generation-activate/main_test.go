package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/efiboot"
	"github.com/katl-dev/katl/internal/generation"
	"golang.org/x/sys/unix"
)

func TestGenerationActivateUsesSelectedMetadata(t *testing.T) {
	root := t.TempDir()
	useTestMover(t)
	record := commandActivationRecord(t, root, "2026.06.05-001")
	metadataPath, err := generation.MetadataPath(root, record.GenerationID)
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	if err := generation.WriteRecord(metadataPath, record); err != nil {
		t.Fatalf("WriteRecord() error = %v", err)
	}
	cmdline := filepath.Join(root, "proc-cmdline")
	if err := os.WriteFile(cmdline, []byte("root=PARTUUID=abc katl.generation=2026.06.05-001\n"), 0o644); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}

	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	assertCommandSymlink(t, filepath.Join(root, "run/extensions/kubernetes.raw"), record.Sysexts[0].Path)
	assertCommandSymlink(t, filepath.Join(root, "run/confexts/katl-node"), record.Confexts[0].Path)
	if !strings.Contains(stdout.String(), "generation=2026.06.05-001") {
		t.Fatalf("stdout = %q, want selected generation", stdout.String())
	}
}

func TestGenerationActivateRejectsMismatchedMetadata(t *testing.T) {
	root := t.TempDir()
	record := commandActivationRecord(t, root, "2026.06.05-001")
	metadataPath, err := generation.MetadataPath(root, record.GenerationID)
	if err != nil {
		t.Fatalf("MetadataPath() error = %v", err)
	}
	record.GenerationID = "2026.06.05-002"
	data, err := generation.MarshalRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	err = run(t.Context(), []string{"--root", root, "--generation", "2026.06.05-001"}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match selected generation") {
		t.Fatalf("run() error = %v, want metadata mismatch", err)
	}
}

func TestGenerationActivateStartsTrialDeadline(t *testing.T) {
	for _, tc := range []struct {
		name             string
		running          string
		unreadableUKI    bool
		armErr           error
		wantArmed        bool
		wantErr, wantOut string
	}{
		{name: "bound trial", running: "candidate", wantArmed: true, wantOut: "trialBound=true"},
		// Activation defers fallback checks to boot health, so an unreadable
		// fallback never stops the candidate from running under its deadline.
		{name: "unreadable fallback", running: "candidate", unreadableUKI: true, wantArmed: true, wantOut: "trialBound=true"},
		{name: "deadline fails", running: "candidate", armErr: errors.New("unit not loaded"),
			wantArmed: true, wantErr: "could not start the boot deadline for the trial of generation candidate: unit not loaded; failing this boot"},
		{name: "fallback boot", running: "known-good", wantOut: "trialBound=false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeActivationTrial(t, root)
			writeActivationGeneration(t, root, "candidate", "known-good", generation.BootStateTrying, generation.HealthStateUnknown, time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC))
			if tc.unreadableUKI {
				uki := filepath.Join(root, "efi/EFI/Linux/katl-known-good.efi")
				if err := os.Remove(uki); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(uki, uki); err != nil {
					t.Fatal(err)
				}
			}
			cmdline := filepath.Join(root, "proc-cmdline")
			writeCommandFile(t, cmdline, "root=PARTUUID=11111111-2222-3333-4444-555555555555 katl.generation="+tc.running+"\n")
			useTestMover(t)
			armed := false
			oldArm := armBootTrial
			armBootTrial = func(context.Context) error { armed = true; return tc.armErr }
			t.Cleanup(func() { armBootTrial = oldArm })

			var stdout bytes.Buffer
			err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("run() error = %v (%s)", err, &stdout)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("run() error = %v, want %q", err, tc.wantErr)
			}
			if !strings.Contains(stdout.String(), tc.wantOut) {
				t.Fatalf("stdout = %q, want %q", &stdout, tc.wantOut)
			}
			if armed != tc.wantArmed {
				t.Fatalf("deadline armed = %t, want %t", armed, tc.wantArmed)
			}
		})
	}
}

// useTestMover gives activation a fixed boot ID and EFI variables that match
// the published default. Activation must not write them.
func useTestMover(t *testing.T) {
	t.Helper()
	old := newBootMover
	newBootMover = func(root string, options generation.BootMoverOptions) (*generation.BootMover, error) {
		options.BootID = "boot-2"
		options.ReadEFI = func(string) (efiboot.Entries, error) {
			selection, err := generation.ReadBootSelection(root)
			return efiboot.Entries{Default: filepath.Base(selection.DefaultBootEntry), OneShot: filepath.Base(selection.TrialBootEntry)}, err
		}
		write := func(_, entry string) error {
			t.Errorf("activation wrote EFI entry %q", entry)
			return nil
		}
		options.SetDefault, options.SetOneShot = write, write
		return old(root, options)
	}
	t.Cleanup(func() { newBootMover = old })
}

func writeActivationTrial(t *testing.T, root string) {
	t.Helper()
	now := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	writeActivationFallback(t, root, "known-good", now.Add(-time.Hour))
	writeActivationDisk(t, root)
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:                    generation.APIVersion,
		Kind:                          generation.BootSelectionKind,
		DefaultGenerationID:           "known-good",
		TargetBootGenerationID:        "candidate",
		TrialGenerationID:             "candidate",
		PreviousKnownGoodGenerationID: "known-good",
		DefaultBootEntry:              "loader/entries/katl-known-good.conf",
		TargetBootEntry:               "loader/entries/katl-candidate.conf",
		TrialBootEntry:                "loader/entries/katl-candidate.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-known-good.conf",
		PendingTransactionID:          "next-boot-apply",
		PendingHealthValidation:       true,
		PersistentDefaultPromotion:    generation.DefaultPromotionPending,
		UpdatedAt:                     now,
	}); err != nil {
		t.Fatal(err)
	}
}

func writeActivationDisk(t *testing.T, root string) {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(root, &stat); err != nil {
		t.Fatal(err)
	}
	sysfs := filepath.Join(root, "sys")
	device := fmt.Sprintf("%d:%d", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)))
	disk := filepath.Join(sysfs, "devices/vda")
	writeCommandFile(t, filepath.Join(disk, "diskseq"), "17\n")
	for n, label := range []string{"KATL_ROOT_A", "KATL_ESP", "KATL_STATE"} {
		dir := filepath.Join(disk, fmt.Sprintf("vda%d", n+1))
		writeCommandFile(t, filepath.Join(dir, "partition"), fmt.Sprint(n+1))
		writeCommandFile(t, filepath.Join(dir, "uevent"), fmt.Sprintf("PARTN=%d\nPARTNAME=%s\nPARTUUID=%d1111111-2222-3333-4444-555555555555\n", n+1, label, n+1))
	}
	for _, path := range []string{"dev/block/" + device, "class/block/vda1"} {
		path = filepath.Join(sysfs, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(disk, "vda1"), path); err != nil {
			t.Fatal(err)
		}
	}
	writeCommandFile(t, filepath.Join(root, "dev/disk/by-diskseq/17-part1"), string(activationRuntimeImage()))
}

func TestGenerationActivateDuringSlotInvalidation(t *testing.T) {
	fixture, err := os.ReadFile("../../internal/installer/testdata/persisted/v1/katl.boot.move-intent.json")
	if err != nil {
		t.Fatal(err)
	}
	const unavailable = "OS slot has been replaced"
	intent := strings.Replace(string(fixture), `"unavailableReason": ""`, `"unavailableReason": "`+unavailable+`"`, 1)
	now := time.Date(2026, 11, 26, 14, 59, 0, 0, time.UTC)
	for _, tc := range []struct{ name, operation, uuid, wantError string }{
		{"intact prior target", "recover-gen1", "11111111-2222-3333-4444-555555555555", ""},
		{"unrelated invalidation", "other-operation", "11111111-2222-3333-4444-555555555555", unavailable},
		{"wrong running root", "recover-gen1", "21111111-2222-3333-4444-555555555555", "does not match generation root PARTUUID"},
		{"missing bound slot", "recover-gen1", "", "is absent from the system disk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeActivationDisk(t, root)
			useTestMover(t)
			for _, id := range []string{"gen0", "gen1", "gen2", "gen3"} {
				writeActivationFallback(t, root, id, now.Add(-time.Minute))
			}
			spec, status, err := generation.ReadGeneration(root, "gen1")
			if err != nil {
				t.Fatal(err)
			}
			status.UnavailableReason = unavailable
			status.UpdatedAt = now
			status.StatusTransitions = append(status.StatusTransitions, generation.StatusTransition{At: now, OperationID: tc.operation, Reason: "trial failed"})
			if err := generation.WriteGenerationStatus(root, spec, status); err != nil {
				t.Fatal(err)
			}
			writeCommandFile(t, filepath.Join(root, "var/lib/katl/boot/move-intent.json"), intent)
			uuid := tc.uuid
			if uuid == "" {
				uuid = spec.Root.PartitionUUID
				writeCommandFile(t, filepath.Join(root, "sys/devices/vda/vda1/uevent"), "PARTN=1\nPARTNAME=KATL_ROOT_A\nPARTUUID=41111111-2222-3333-4444-555555555555\n")
			}
			cmdline := filepath.Join(root, "proc-cmdline")
			writeCommandFile(t, cmdline, "root=PARTUUID="+uuid+" katl.generation=gen1\n")

			err = run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, nil)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
				if id, recorded, err := generation.CurrentBootGeneration(root); err != nil || !recorded || id != "gen1" {
					t.Fatalf("activation recorded %q, %t, %v", id, recorded, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("activation = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestGenerationActivateRecordsFallbackBootIdentity(t *testing.T) {
	root := t.TempDir()
	useTestMover(t)
	now := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	writeActivationFallback(t, root, "known-good", now.Add(-time.Hour))
	if err := generation.WriteBootSelection(root, generation.BootSelectionRecord{
		APIVersion:                    generation.APIVersion,
		Kind:                          generation.BootSelectionKind,
		DefaultGenerationID:           "known-good",
		PreviousKnownGoodGenerationID: "known-good",
		BootedGenerationID:            "failed-trial",
		FailedBootGenerationID:        "failed-trial",
		DefaultBootEntry:              "loader/entries/katl-known-good.conf",
		PreviousKnownGoodBootEntry:    "loader/entries/katl-known-good.conf",
		BootedBootEntry:               "loader/entries/katl-failed-trial.conf",
		UpdatedAt:                     now,
	}); err != nil {
		t.Fatal(err)
	}
	cmdline := filepath.Join(root, "proc-cmdline")
	if err := os.WriteFile(cmdline, []byte("root=PARTUUID=11111111-2222-3333-4444-555555555555 katl.generation=known-good\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, nil); err != nil {
		t.Fatal(err)
	}
	selection, err := generation.ReadBootSelection(root)
	if err != nil {
		t.Fatal(err)
	}
	if selection.BootedGenerationID != "known-good" || selection.BootedBootEntry != "loader/entries/katl-known-good.conf" || selection.FailedBootGenerationID != "failed-trial" {
		t.Fatalf("boot selection after activation = %#v", selection)
	}
}

func TestActivateHostnameUsesSelectedVerifiedConfext(t *testing.T) {
	root := t.TempDir()
	confext := "/var/lib/katl/generations/1/confext"
	writeCommandFile(t, filepath.Join(root, strings.TrimPrefix(confext, "/"), "etc/hostname"), "cp-1\n")
	var selected string
	hostname, err := activateHostname(root, generation.ActivationPlan{
		Confexts: []generation.ActivationLink{{Name: "katl-node", SourcePath: confext}},
	}, func(value []byte) error {
		selected = string(value)
		return nil
	})
	if err != nil {
		t.Fatalf("activateHostname() error = %v", err)
	}
	if hostname != "cp-1" || selected != "cp-1" {
		t.Fatalf("hostname = %q, selected = %q", hostname, selected)
	}
}

func TestActivateHostnameKeepsBaseHostnameForOlderGeneration(t *testing.T) {
	called := false
	hostname, err := activateHostname(t.TempDir(), generation.ActivationPlan{}, func([]byte) error {
		called = true
		return nil
	})
	if err != nil || hostname != "" || called {
		t.Fatalf("activateHostname() hostname = %q, called = %t, error = %v", hostname, called, err)
	}
}

func commandActivationRecord(t *testing.T, root string, id string) generation.Record {
	t.Helper()
	sysextContent := "selected sysext\n"
	sysextPath := filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, id, "sysext", "kubernetes.raw"))
	confextPath := filepath.ToSlash(filepath.Join(generation.GenerationRecordsDir, id, "confext"))
	writeCommandFile(t, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(sysextPath, "/"))), sysextContent)
	writeCommandFile(t, filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(confextPath, "/")), "etc/hostname"), "katl-node\n")
	confextDigest, err := generation.DigestDirectory(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(confextPath, "/"))))
	if err != nil {
		t.Fatalf("DigestDirectory() error = %v", err)
	}
	sum := sha256.Sum256([]byte(sysextContent))
	return generation.Record{
		APIVersion:   generation.APIVersion,
		Kind:         generation.Kind,
		GenerationID: id,
		Root: generation.RootSelection{
			RuntimeInterface: "katl-runtime-1",
			Architecture:     "x86_64",
		},
		Sysexts: []generation.ExtensionRef{
			{
				Name:            "kubernetes",
				Path:            sysextPath,
				ActivationPath:  "/run/extensions/kubernetes.raw",
				SHA256:          hex.EncodeToString(sum[:]),
				ArtifactVersion: "k8s-v1.34.8",
				PayloadVersion:  "v1.34.8",
				Architecture:    "x86_64",
				Compatibility: generation.ExtensionCompatibility{
					RuntimeInterfaces: []string{"katl-runtime-1"},
				},
			},
		},
		Confexts: []generation.GeneratedConfext{
			{
				Name:           "katl-node",
				Path:           confextPath,
				ActivationPath: "/run/confexts/katl-node",
				SHA256:         confextDigest,
				Compatibility: generation.ConfextCompatibility{
					ID:           "katl",
					VersionID:    "0.1.0",
					ConfextLevel: 1,
				},
			},
		},
	}
}

func writeActivationFallback(t *testing.T, root string, id string, updatedAt time.Time) {
	t.Helper()
	writeActivationGeneration(t, root, id, "", generation.BootStateGood, generation.HealthStateHealthy, updatedAt)
}

func writeActivationGeneration(t *testing.T, root, id, previous, bootState, healthState string, updatedAt time.Time) {
	t.Helper()
	spec := generation.GenerationSpec{
		APIVersion:     generation.APIVersion,
		Kind:           generation.SpecKind,
		GenerationID:   id,
		RuntimeVersion: "0.1.0",
		Root: generation.RootSelection{
			Slot:                  "root-a",
			PartitionUUID:         "11111111-2222-3333-4444-555555555555",
			RuntimeVersion:        "0.1.0",
			RuntimeInterface:      "katl-runtime-1",
			Architecture:          "x86_64",
			RuntimeArtifactSHA256: fmt.Sprintf("%x", sha256.Sum256(activationRuntimeImage())),
		},
		Boot: generation.BootSelection{
			UKIPath:         "/efi/EFI/Linux/katl-" + id + ".efi",
			LoaderEntryPath: "loader/entries/katl-" + id + ".conf",
		},
		PreviousGenerationID: previous,
		CreatedAt:            updatedAt,
	}
	status, err := generation.NewGenerationStatus(spec, generation.CommitStateCommitted, bootState, healthState, updatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{spec.Boot.UKIPath, filepath.Join("/efi", spec.Boot.LoaderEntryPath)} {
		writeCommandFile(t, filepath.Join(root, strings.TrimPrefix(path, "/")), id)
	}
}

func activationRuntimeImage() []byte {
	image := make([]byte, 4096)
	copy(image, "hsqs")
	binary.LittleEndian.PutUint64(image[40:48], 96)
	return image
}

func writeCommandFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertCommandSymlink(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	if got != want {
		t.Fatalf("%s -> %s, want %s", path, got, want)
	}
}

// Activation falls through to observation when reconciliation fails. While
// KatlOS 2026.11.5 runs a bootable fallback, that publish carries no field
// 2026.11.5 rejects.
func TestGenerationActivateObservesInLegacyForm(t *testing.T) {
	root := t.TempDir()
	writeActivationTrial(t, root)
	writeActivationGeneration(t, root, "candidate", "known-good", generation.BootStateTrying, generation.HealthStateUnknown, time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC))
	spec, status, err := generation.ReadGeneration(root, "known-good")
	if err != nil {
		t.Fatal(err)
	}
	spec.RuntimeVersion = "2026.11.5"
	if status.SpecDigest, err = generation.CanonicalSpecDigest(spec); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"spec.json", "status.json"} {
		if err := os.Remove(filepath.Join(root, "var/lib/katl/generations/known-good", name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := generation.WriteGeneration(root, spec, status); err != nil {
		t.Fatal(err)
	}
	cmdline := filepath.Join(root, "proc-cmdline")
	writeCommandFile(t, cmdline, "root=PARTUUID=11111111-2222-3333-4444-555555555555 katl.generation=candidate\n")
	old := newBootMover
	newBootMover = func(root string, options generation.BootMoverOptions) (*generation.BootMover, error) {
		options.BootID = "boot-2"
		options.ReadEFI = func(string) (efiboot.Entries, error) { return efiboot.Entries{}, errors.New("efivarfs unavailable") }
		return old(root, options)
	}
	t.Cleanup(func() { newBootMover = old })
	oldArm := armBootTrial
	armBootTrial = func(context.Context) error { return nil }
	t.Cleanup(func() { armBootTrial = oldArm })

	var stdout bytes.Buffer
	if err := run(t.Context(), []string{"--root", root, "--cmdline", cmdline}, &stdout); err != nil {
		t.Fatalf("run() error = %v (%s)", err, &stdout)
	}

	if !strings.Contains(stdout.String(), "could not reconcile boot state") {
		t.Fatalf("stdout = %q, want a reconcile error", &stdout)
	}
	data, err := os.ReadFile(filepath.Join(root, "var/lib/katl/boot/selection.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"bootedGenerationID": "candidate"`) || strings.Contains(string(data), "revision") {
		t.Fatalf("selection after activation =\n%s\nwant candidate observed without a revision", data)
	}
}
