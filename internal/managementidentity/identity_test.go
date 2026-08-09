package managementidentity

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEnsureNodeReusesStableLeafAndPinsItsName(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	bundle, err := Generate(GenerateOptions{ClusterName: "lab", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	first, added, err := EnsureNode(&bundle, "cp-1", now, nil)
	if err != nil || !added {
		t.Fatalf("first EnsureNode() added=%v err=%v", added, err)
	}
	second, added, err := EnsureNode(&bundle, "cp-1", now, nil)
	if err != nil || added {
		t.Fatalf("second EnsureNode() added=%v err=%v", added, err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("node identity changed between repeated planning")
	}
	if err := ValidateNode(first, "cp-2", now); err == nil || !strings.Contains(err.Error(), "cp-2") {
		t.Fatalf("ValidateNode() wrong-name error = %v", err)
	}
}

func TestWriteProtectsIdentityAndRefusesOverwrite(t *testing.T) {
	now := time.Now().UTC()
	bundle, err := Generate(GenerateOptions{ClusterName: "lab", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "management", "lab.katlkey")
	if err := Write(path, bundle); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity mode = %04o", info.Mode().Perm())
	}
	if err := Write(path, bundle); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second Write() error = %v", err)
	}
	if _, _, err := EnsureNode(&bundle, "cp-1", now, nil); err != nil {
		t.Fatal(err)
	}
	if err := SaveExisting(path, bundle); err != nil {
		t.Fatal(err)
	}
	got, _, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Nodes["cp-1"]; !ok {
		t.Fatal("saved identity did not retain issued node")
	}
}
