package configbundle

import (
	"strings"
	"testing"
)

func TestDiffNodeResolutionsRefusesDifferentNodeNames(t *testing.T) {
	_, err := DiffNodeResolutions(
		NodeResolution{Node: "cp-1"},
		NodeResolution{Node: "cp-2"},
	)
	if err == nil || !strings.Contains(err.Error(), `different nodes "cp-1" and "cp-2"`) || !strings.Contains(err.Error(), "node renames require an explicit lifecycle operation") {
		t.Fatalf("DiffNodeResolutions() error = %v, want lifecycle refusal", err)
	}
}
