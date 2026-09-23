package operation

import (
	"strings"
	"testing"
)

func TestMutationLockExcludesConcurrentOwners(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.AcquireMutationLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireMutationLock(); err == nil || !strings.Contains(err.Error(), "another node mutation") {
		t.Fatalf("concurrent acquisition = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.AcquireMutationLock()
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
