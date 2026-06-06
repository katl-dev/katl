package resourcetest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPackageLockRoundTrip(t *testing.T) {
	lock := validPackageLock()
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	decoded, err := DecodePackageLock(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("DecodePackageLock() error = %v", err)
	}
	if decoded.MkosiProfiles[0].Name != lock.MkosiProfiles[0].Name {
		t.Fatalf("decoded lock = %#v", decoded)
	}
}

func TestVerifyPackageLockMatchesManifest(t *testing.T) {
	lock := validPackageLock()
	digest := digestForLock(t, lock)
	manifest := manifestForPackageLock(digest)

	if err := VerifyPackageLock(PackageLockVerification{Lock: lock, Manifest: manifest, LockDigest: digest}); err != nil {
		t.Fatalf("VerifyPackageLock() error = %v", err)
	}
}

func TestVerifyPackageLockRejectsPackageDrift(t *testing.T) {
	lock := validPackageLock()
	digest := digestForLock(t, lock)
	manifest := manifestForPackageLock(digest)
	manifest.PackageSets[0].Packages[0].NEVRA = "systemd-0:259-2.x86_64"

	err := VerifyPackageLock(PackageLockVerification{Lock: lock, Manifest: manifest, LockDigest: digest})
	if err == nil || !strings.Contains(err.Error(), "NEVRA drift") {
		t.Fatalf("VerifyPackageLock() error = %v, want NEVRA drift", err)
	}
}

func TestVerifyPackageLockRejectsRepositoryDrift(t *testing.T) {
	lock := validPackageLock()
	digest := digestForLock(t, lock)
	manifest := manifestForPackageLock(digest)
	manifest.PackageSets[0].Repositories[0].BaseURL = "https://example.invalid/changed"

	err := VerifyPackageLock(PackageLockVerification{Lock: lock, Manifest: manifest, LockDigest: digest})
	if err == nil || !strings.Contains(err.Error(), "baseURL drift") {
		t.Fatalf("VerifyPackageLock() error = %v, want baseURL drift", err)
	}
}

func TestVerifyPackageLockRejectsUnlockedPackageSet(t *testing.T) {
	lock := validPackageLock()
	digest := digestForLock(t, lock)
	manifest := manifestForPackageLock(digest)
	manifest.PackageSets = append(manifest.PackageSets, PackageSet{
		Name:       "katlos-install-image",
		LockDigest: digest,
		Repositories: []PackageRepository{{
			ID: "katlos-components",
		}},
		Packages: []Package{{
			Name:     "katlos-component-runtime-root",
			NEVRA:    "runtime-root-0.0.0.x86_64",
			Checksum: testSHA,
		}},
	})

	err := VerifyPackageLock(PackageLockVerification{Lock: lock, Manifest: manifest, LockDigest: digest})
	if err == nil || !strings.Contains(err.Error(), "package set \"katlos-install-image\" is missing from package lock") {
		t.Fatalf("VerifyPackageLock() error = %v, want unlocked package set", err)
	}
}

func TestVerifyPackageLockRejectsToolDrift(t *testing.T) {
	lock := validPackageLock()
	digest := digestForLock(t, lock)
	manifest := manifestForPackageLock(digest)
	manifest.Tools[0].Version = "27"

	err := VerifyPackageLock(PackageLockVerification{Lock: lock, Manifest: manifest, LockDigest: digest})
	if err == nil || !strings.Contains(err.Error(), "tool \"mkosi\" version drift") {
		t.Fatalf("VerifyPackageLock() error = %v, want mkosi version drift", err)
	}
}

func TestVerifyPackageLockRejectsMissingLockData(t *testing.T) {
	lock := validPackageLock()
	lock.PackageSets = nil
	digest := digestForLock(t, validPackageLock())

	err := VerifyPackageLock(PackageLockVerification{Lock: lock, Manifest: manifestForPackageLock(digest), LockDigest: digest})
	if err == nil || !strings.Contains(err.Error(), "packageSets is required") {
		t.Fatalf("VerifyPackageLock() error = %v, want missing packageSets", err)
	}
}

func TestVerifyPackageLockRejectsStaleLockDigest(t *testing.T) {
	lock := validPackageLock()
	digest := digestForLock(t, lock)
	manifest := manifestForPackageLock(strings.Repeat("b", 64))

	err := VerifyPackageLock(PackageLockVerification{Lock: lock, Manifest: manifest, LockDigest: digest})
	if err == nil || !strings.Contains(err.Error(), "lock digest drift") {
		t.Fatalf("VerifyPackageLock() error = %v, want lock digest drift", err)
	}
}

func TestVerifyPackageLockRejectsMissingManifestLockDigest(t *testing.T) {
	lock := validPackageLock()
	digest := digestForLock(t, lock)
	manifest := manifestForPackageLock("")

	err := VerifyPackageLock(PackageLockVerification{Lock: lock, Manifest: manifest, LockDigest: digest})
	if err == nil || !strings.Contains(err.Error(), "lock digest drift") {
		t.Fatalf("VerifyPackageLock() error = %v, want lock digest drift", err)
	}
}

func TestPackageLockFromManifest(t *testing.T) {
	manifest := manifestForPackageLock("")
	lock, err := PackageLockFromManifest(manifest)
	if err != nil {
		t.Fatalf("PackageLockFromManifest() error = %v", err)
	}
	if lock.Kind != PackageLockKind || lock.PackageSets[0].Repositories[0].ID != "fedora" {
		t.Fatalf("lock = %#v", lock)
	}
	if lock.MkosiProfiles[0].MkosiVersion != "26" {
		t.Fatalf("lock mkosiVersion = %q, want 26", lock.MkosiProfiles[0].MkosiVersion)
	}
}

func TestPackageLockFromManifestRejectsMissingRepositoryData(t *testing.T) {
	manifest := manifestForPackageLock("")
	manifest.PackageSets[0].Repositories = nil

	_, err := PackageLockFromManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), "repositories are required") {
		t.Fatalf("PackageLockFromManifest() error = %v, want repository requirement", err)
	}
}

func TestValidatePackageLockRejectsInvalidChecksum(t *testing.T) {
	lock := validPackageLock()
	lock.PackageSets[0].Packages[0].Checksum = strings.ToUpper(testSHA)

	err := ValidatePackageLock(lock)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("ValidatePackageLock() error = %v, want sha256 rejection", err)
	}
}

func validPackageLock() PackageLock {
	return PackageLock{
		APIVersion: APIVersion,
		Kind:       PackageLockKind,
		Tools: []Tool{{
			Name:    "mkosi",
			Version: "26",
		}, {
			Name:    "go",
			Version: "go1.26",
		}},
		MkosiProfiles: []PackageLockProfile{{
			Name:          "runtime",
			Path:          "mkosi.profiles/runtime",
			ConfigDigest:  testSHA,
			PackageSetRef: "runtime",
			MkosiVersion:  "26",
		}},
		PackageSets: []PackageLockPackageSet{{
			Name:         "runtime",
			Source:       "mkosi.profiles/runtime",
			Distribution: "fedora",
			Release:      "44",
			Architecture: "x86_64",
			Repositories: []PackageRepository{{
				ID:      "fedora",
				BaseURL: "https://example.invalid/fedora/44",
			}},
			Packages: []Package{{
				Name:     "systemd",
				NEVRA:    "systemd-0:259.6-1.fc44.x86_64",
				Checksum: testSHA,
			}},
		}},
	}
}

func manifestForPackageLock(lockDigest string) Manifest {
	manifest := validManifest()
	manifest.Tools = []Tool{{
		Name:    "mkosi",
		Version: "26",
	}, {
		Name:    "go",
		Version: "go1.26",
	}}
	manifest.PackageSets[0] = PackageSet{
		Name:         "runtime",
		Source:       "mkosi.profiles/runtime",
		Digest:       testSHA,
		LockDigest:   lockDigest,
		Distribution: "fedora",
		Release:      "44",
		Architecture: "x86_64",
		Repositories: []PackageRepository{{
			ID:      "fedora",
			BaseURL: "https://example.invalid/fedora/44",
		}},
		Packages: []Package{{
			Name:     "systemd",
			NEVRA:    "systemd-0:259.6-1.fc44.x86_64",
			Checksum: testSHA,
		}},
	}
	manifest.MkosiProfiles[0] = MkosiProfile{
		Name:          "runtime",
		Path:          "mkosi.profiles/runtime",
		ConfigDigest:  testSHA,
		PackageSetRef: "runtime",
	}
	return manifest
}

func digestForLock(t *testing.T, lock PackageLock) string {
	t.Helper()
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return PackageLockDigest(data)
}
