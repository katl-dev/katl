package disk

import (
	"testing"
)

func TestValidateDestructiveVolumeAcknowledgementKeys(t *testing.T) {
	if err := ValidateDestructiveVolumeAcknowledgementKeys([]string{"cp-1/data", "worker-1/cache"}); err != nil {
		t.Fatalf("valid acknowledgement keys rejected: %v", err)
	}
	for _, values := range [][]string{{"cp-1"}, {"CP-1/data"}, {"cp-1/data/extra"}, {"cp-1/data", "cp-1/data"}} {
		if err := ValidateDestructiveVolumeAcknowledgementKeys(values); err == nil {
			t.Fatalf("invalid acknowledgement keys accepted: %v", values)
		}
	}
}
