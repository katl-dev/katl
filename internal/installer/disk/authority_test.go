package disk

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDestructiveVolumeAcknowledgementsOnlyCoverNonBlankTargets(t *testing.T) {
	plans := []VolumePlan{
		{Name: "blank-disk", Wipe: true, Repartition: true},
		{Name: "existing-partition", Wipe: true, Signatures: []SignatureReport{{Kind: "filesystem", Value: "ext4"}}},
		{Name: "existing-disk", Wipe: true, Repartition: true, Signatures: []SignatureReport{{Kind: "partition-table", Value: "gpt"}}},
		{Name: "preserved", Signatures: []SignatureReport{{Kind: "filesystem", Value: "xfs"}}},
	}
	want := []string{"cp-1/existing-disk", "cp-1/existing-partition"}
	if got := RequiredDestructiveVolumeAcknowledgements("cp-1", plans); !reflect.DeepEqual(got, want) {
		t.Fatalf("RequiredDestructiveVolumeAcknowledgements() = %v, want %v", got, want)
	}

	err := ValidateDestructiveVolumeAcknowledgements("cp-1", plans, []string{"cp-1/existing-disk"})
	var authority *DestructiveVolumeAuthorityError
	if !errors.As(err, &authority) || !reflect.DeepEqual(authority.Required, []string{"cp-1/existing-partition"}) {
		t.Fatalf("ValidateDestructiveVolumeAcknowledgements() error = %#v", err)
	}
	if !strings.Contains(err.Error(), "--acknowledge-storage-wipe cp-1/existing-partition") {
		t.Fatalf("authority error lacks retry command: %v", err)
	}
	if err := ValidateDestructiveVolumeAcknowledgements("cp-1", plans, want); err != nil {
		t.Fatalf("acknowledged validation error = %v", err)
	}
}

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
